package mux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LiZeC123/gks/internal/log"
	"github.com/LiZeC123/gks/internal/protocol"
)

// 流相关错误。
var (
	// ErrStreamReset 表示流被 RST。
	ErrStreamReset = errors.New("mux: 流被重置")
	// ErrStreamWriteClosed 表示本端已发 FIN，不能再写。
	ErrStreamWriteClosed = errors.New("mux: 本方向已关闭")
	// ErrStreamExists 表示 StreamID 已被占用。
	ErrStreamExists = errors.New("mux: StreamID 已存在")
	// ErrBadStreamID 表示 StreamID 非法（0 保留给控制帧）。
	ErrBadStreamID = errors.New("mux: StreamID 非法")
	// ErrStreamRecvStalled 表示接收缓冲长时间满，流被重置。
	ErrStreamRecvStalled = errors.New("mux: 流接收缓冲长时间满，已重置")
)

// recvQueue 是单流的接收缓冲：带字节上限（背压）+ EOF/错误语义。
//
// 用 channel 而非 sync.Cond 做通知，因为 push 需要带超时等待：
// 若某个流长时间不被消费，宁可重置该流，也不能永久卡住会话读循环（dev.md §3.7）。
type recvQueue struct {
	mu      sync.Mutex
	buf     []byte
	limit   int
	eof     bool
	err     error
	discard bool

	signal chan struct{} // 有数据/状态变化，唤醒 Read
	space  chan struct{} // 有空间，唤醒 push
}

func newRecvQueue(limit int) *recvQueue {
	if limit <= 0 {
		limit = 256 * 1024
	}
	return &recvQueue{
		limit:  limit,
		signal: make(chan struct{}, 1),
		space:  make(chan struct{}, 1),
	}
}

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// push 追加数据。缓冲已满时最多等待 timeout；超时返回 false（调用方应重置该流）。
//
// 流已关闭/被标记丢弃时返回 true（静默丢弃，不视为失败）。
func (q *recvQueue) push(b []byte, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		q.mu.Lock()
		if q.err != nil || q.eof || q.discard {
			q.mu.Unlock()
			return true
		}
		if len(q.buf)+len(b) <= q.limit {
			q.buf = append(q.buf, b...)
			q.mu.Unlock()
			notify(q.signal)
			return true
		}
		q.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-q.space:
			timer.Stop()
		case <-timer.C:
			return false
		}
	}
}

// Read 读取已到达的数据；对端 FIN 后读空则返回 io.EOF。
func (q *recvQueue) Read(p []byte) (int, error) {
	for {
		q.mu.Lock()
		if len(q.buf) > 0 {
			n := copy(p, q.buf)
			q.buf = q.buf[n:]
			if len(q.buf) == 0 {
				q.buf = nil
			}
			q.mu.Unlock()
			notify(q.space)
			return n, nil
		}
		if q.err != nil {
			err := q.err
			q.mu.Unlock()
			return 0, err
		}
		if q.discard || q.eof {
			q.mu.Unlock()
			return 0, io.EOF
		}
		q.mu.Unlock()

		<-q.signal
	}
}

// closeEOF 标记对端已发 FIN：已到达的数据仍可读完，之后返回 io.EOF。
func (q *recvQueue) closeEOF() {
	q.mu.Lock()
	q.eof = true
	q.mu.Unlock()
	notify(q.signal)
	notify(q.space)
}

// closeWith 以错误终止接收（RST / 会话关闭）。
func (q *recvQueue) closeWith(err error) {
	q.mu.Lock()
	if q.err == nil {
		q.err = err
	}
	q.mu.Unlock()
	notify(q.signal)
	notify(q.space)
}

// startDiscard 丢弃后续到达的数据（本端不再读取）。
func (q *recvQueue) startDiscard() {
	q.mu.Lock()
	q.discard = true
	q.mu.Unlock()
	notify(q.signal)
	notify(q.space)
}

// stream 是一条逻辑流。语义见 dev.md §3.8 的状态机。
type stream struct {
	id   uint32
	sess *Session
	recv *recvQueue

	writeMu sync.Mutex // 串行化分片写入，保证同一流内 DATA 顺序

	localEOF  atomic.Bool // 已发 FIN
	remoteEOF atomic.Bool // 已收 FIN
	reset     atomic.Bool

	closed    chan struct{}
	closeOnce sync.Once
	onClose   func()

	sentBytes atomic.Uint64
	recvBytes atomic.Uint64
}

func newStream(id uint32, s *Session) *stream {
	return &stream{
		id:     id,
		sess:   s,
		recv:   newRecvQueue(recvQueueLimit),
		closed: make(chan struct{}),
	}
}

// recvQueueLimit 是单流接收缓冲上限（dev.md §3.7：256KB）。
const recvQueueLimit = 256 * 1024

var _ Stream = (*stream)(nil)

// StreamID 返回流标识。
func (st *stream) StreamID() uint32 { return st.id }

// Read 读取对端发来的数据。
//
// 载荷字节实时累加到进程计数器，这样它与传输层的线速率为同一时间窗口口径，
// 便于直接对比（dev.md §9.5 的速率观测）。
func (st *stream) Read(p []byte) (int, error) {
	n, err := st.recv.Read(p)
	if n > 0 {
		st.recvBytes.Add(uint64(n))
		st.sess.opts.Metrics.PayloadReceived.Add(uint64(n))
	}
	return n, err
}

// Write 把数据拆成不超过 MaxDataPayload 的 DATA 帧发出。
//
// 发送队列满时会阻塞（背压），直到队列有空间或会话关闭。
func (st *stream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if st.reset.Load() {
		return 0, ErrStreamReset
	}
	if st.localEOF.Load() {
		return 0, ErrStreamWriteClosed
	}
	st.writeMu.Lock()
	defer st.writeMu.Unlock()

	chunk := st.sess.opts.MaxDataPayload
	written := 0
	for written < len(p) {
		end := written + chunk
		if end > len(p) {
			end = len(p)
		}
		payload := p[written:end]
		if err := st.sess.SendFrame(context.Background(), protocol.TypeData, st.id, payload); err != nil {
			return written, err
		}
		written = end
	}
	st.sentBytes.Add(uint64(written))
	st.sess.opts.Metrics.PayloadSent.Add(uint64(written))
	return written, nil
}

// CloseWrite 发送 FIN：本方向不再发送 DATA，但仍可接收（半关闭）。
func (st *stream) CloseWrite() error {
	if st.reset.Load() {
		return ErrStreamReset
	}
	if st.localEOF.Swap(true) {
		return nil // 幂等
	}
	if err := st.sess.SendFrame(context.Background(), protocol.TypeFin, st.id, nil); err != nil {
		return err
	}
	st.maybeFinish()
	return nil
}

// CloseRead 关闭本地读方向：对端仍可发送，但本端丢弃后续数据。
func (st *stream) CloseRead() error {
	if st.reset.Load() {
		return ErrStreamReset
	}
	st.recv.startDiscard()
	st.maybeFinish()
	return nil
}

// Reset 发送 RST 并立即双向关闭。
func (st *stream) Reset() error {
	if st.reset.Swap(true) {
		return nil
	}
	err := st.sess.SendFrame(context.Background(), protocol.TypeRst, st.id, nil)
	st.finish(ErrStreamReset)
	return err
}

// Close 等价于 CloseWrite + CloseRead。
func (st *stream) Close() error {
	err := st.CloseWrite()
	_ = st.CloseRead()
	return err
}

// 以下是 Session 侧的内部调用。

// deliver 写入对端数据；返回 false 表示缓冲长时间满，调用方应重置该流。
func (st *stream) deliver(payload []byte, timeout time.Duration) bool {
	if st.reset.Load() {
		return true
	}
	return st.recv.push(payload, timeout)
}

// deliverFin 处理对端 FIN。
func (st *stream) deliverFin() {
	if st.reset.Load() {
		return
	}
	st.remoteEOF.Store(true)
	st.recv.closeEOF()
	st.maybeFinish()
}

// forceReset 由对端 RST 或会话关闭触发：不再发送任何帧。
func (st *stream) forceReset(err error) {
	st.reset.Store(true)
	st.finish(err)
}

// maybeFinish 在两个方向都结束后释放资源。
func (st *stream) maybeFinish() {
	if st.localEOF.Load() && (st.remoteEOF.Load() || st.recv.isDiscard()) {
		st.finish(nil)
	}
}

func (st *stream) finish(err error) {
	st.closeOnce.Do(func() {
		if err != nil {
			st.recv.closeWith(err)
		} else {
			st.recv.closeEOF()
		}
		close(st.closed)
		if st.onClose != nil {
			st.onClose()
		}
		st.sess.log.Debug("流关闭",
			log.Event, "stream_down",
			log.StreamID, st.id,
			"sent_bytes", st.sentBytes.Load(),
			"recv_bytes", st.recvBytes.Load(),
			"err", err,
		)
		// 载荷字节已在 Read/Write 时实时累加，这里只回收活跃流计数。
		st.sess.opts.Metrics.StreamsActive.Add(-1)
	})
}

func (q *recvQueue) isDiscard() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.discard
}

// ---- Session 的流管理 ----

// OpenStream 主动开一条流（仅客户端）。StreamID 单调递增且为奇数，不回收复用。
func (s *Session) OpenStream(ctx context.Context) (Stream, error) {
	if s.role != RoleClient {
		return nil, ErrOpenUnsupported
	}
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if _, ok := s.streams[0]; ok {
		return nil, ErrBadStreamID
	}
	if len(s.streams) >= s.opts.MaxStreams {
		return nil, ErrTooManyStreams
	}
	id := s.nextStreamID
	if id == 0 || id > 0x7FFFFFFF {
		return nil, ErrTooManyStreams
	}
	s.nextStreamID += 2
	return s.newStreamLocked(id), nil
}

// RegisterStream 登记一条由对端发起的流（服务端在处理 CONNECT_REQ 时调用）。
func (s *Session) RegisterStream(id uint32) (Stream, error) {
	if id == 0 {
		return nil, ErrBadStreamID
	}
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if _, ok := s.streams[id]; ok {
		return nil, fmt.Errorf("%w: %d", ErrStreamExists, id)
	}
	if len(s.streams) >= s.opts.MaxStreams {
		return nil, ErrTooManyStreams
	}
	return s.newStreamLocked(id), nil
}

// newStreamLocked 必须在持有 streamsMu 时调用。
func (s *Session) newStreamLocked(id uint32) *stream {
	st := newStream(id, s)
	st.onClose = func() {
		s.streamsMu.Lock()
		delete(s.streams, id)
		s.streamsMu.Unlock()
	}
	s.streams[id] = st
	s.opts.Metrics.StreamsActive.Add(1)
	s.opts.Metrics.StreamsTotal.Add(1)
	s.log.Debug("流建立", log.Event, "stream_up", log.StreamID, id)
	return st
}

// SendFrame 在指定 StreamID 上发送一个帧（应用层发 CONNECT_REQ/CONNECT_RESP 用）。
func (s *Session) SendFrame(ctx context.Context, t protocol.Type, streamID uint32, payload []byte) error {
	return s.send(ctx, protocol.Frame{Type: t, StreamID: streamID, Payload: payload})
}

// NumStreams 返回当前活跃流数。
func (s *Session) NumStreams() int {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	return len(s.streams)
}

func (s *Session) lookupStream(id uint32) *stream {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	return s.streams[id]
}

func (s *Session) resetAllStreams(err error) {
	s.streamsMu.Lock()
	all := make([]*stream, 0, len(s.streams))
	for _, st := range s.streams {
		all = append(all, st)
	}
	s.streamsMu.Unlock()
	for _, st := range all {
		st.forceReset(err)
	}
}

// deliverData 把 DATA 投递给对应流。
func (s *Session) deliverData(f protocol.Frame) {
	st := s.lookupStream(f.StreamID)
	if st == nil {
		s.log.Debug("未知 StreamID 的 DATA，按协议丢弃",
			log.Event, "orphan_data", log.StreamID, f.StreamID, "bytes", len(f.Payload))
		return
	}
	if !st.deliver(f.Payload, s.opts.StreamIdleTimeout) {
		s.log.Warn("流接收缓冲长时间满，重置该流",
			log.Event, "stream_stalled", log.StreamID, f.StreamID,
			"timeout", s.opts.StreamIdleTimeout.String())
		st.forceReset(ErrStreamRecvStalled)
	}
}

// deliverFin 处理 FIN。
func (s *Session) deliverFin(f protocol.Frame) {
	st := s.lookupStream(f.StreamID)
	if st == nil {
		s.log.Debug("未知 StreamID 的 FIN，按协议丢弃", log.Event, "orphan_fin", log.StreamID, f.StreamID)
		return
	}
	st.deliverFin()
}

// deliverRst 处理 RST。
func (s *Session) deliverRst(f protocol.Frame) {
	st := s.lookupStream(f.StreamID)
	if st == nil {
		s.log.Debug("未知 StreamID 的 RST，按协议丢弃", log.Event, "orphan_rst", log.StreamID, f.StreamID)
		return
	}
	s.log.Debug("收到 RST", log.Event, "stream_rst", log.StreamID, f.StreamID)
	st.forceReset(ErrStreamReset)
}
