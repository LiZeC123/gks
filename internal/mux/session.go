package mux

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LiZeC123/gks/internal/log"
	"github.com/LiZeC123/gks/internal/metrics"
	"github.com/LiZeC123/gks/internal/protocol"
)

// 会话相关错误。
var (
	// ErrSessionClosed 表示会话已（被主动）关闭。
	ErrSessionClosed = errors.New("mux: 会话已关闭")
	// ErrHeartbeatTimeout 表示连续多次未收到 PONG，判定会话失效。
	ErrHeartbeatTimeout = errors.New("mux: 心跳超时，会话失效")
	// ErrAuthTimeout 表示认证握手超时。
	ErrAuthTimeout = errors.New("mux: 认证握手超时")
	// ErrUnexpectedFrame 表示握手阶段收到非预期帧类型。
	ErrUnexpectedFrame = errors.New("mux: 握手阶段收到非预期帧")
	// ErrNoPSK 表示缺少 PSK。
	ErrNoPSK = errors.New("mux: 缺少 PSK")
	// ErrNoReplayCache 表示 Server 会话未提供共享重放缓存。
	ErrNoReplayCache = errors.New("mux: Server 会话必须提供共享的重放缓存")
	// ErrPayloadTooLarge 表示单帧 Payload 超过上限。
	ErrPayloadTooLarge = errors.New("mux: 单帧 Payload 超过上限")
)

// heartbeatSendTimeout 是发送 PING/PONG 的写队列等待上限，
// 避免读循环被写阻塞拖住（dev.md §11.2）。
const heartbeatSendTimeout = 5 * time.Second

// Role 是会话在认证握手中的角色。
type Role int

// 角色取值。
const (
	RoleClient Role = iota
	RoleServer
)

func (r Role) String() string {
	switch r {
	case RoleClient:
		return "client"
	case RoleServer:
		return "server"
	default:
		return "unknown"
	}
}

// Handler 处理某一类型的帧。注意 PING/PONG/GOAWAY/ERROR 由 Session 自行处理，
// 不会被 Handler 覆盖。
type Handler func(s *Session, f protocol.Frame)

// Options 是构造 Session 的参数。
type Options struct {
	Role Role
	PSK  []byte
	AEAD string
	// TokenID 为 0 时使用 protocol.DefaultTokenID。
	TokenID uint16
	// AuthTimeout 是等待 AUTH_RESP / AUTH_REQ 的上限。
	AuthTimeout time.Duration
	// TimestampWindow 与 ReplayCache 仅 Server 角色需要。
	TimestampWindow time.Duration
	ReplayCache     *protocol.ReplayCache
	// HeartbeatInterval <= 0 时不启动心跳。
	HeartbeatInterval time.Duration
	// HeartbeatMiss 是连续未收到 PONG 的次数上限，默认 3。
	HeartbeatMiss int
	// WriteQueue 是发送队列长度，默认 1024。
	WriteQueue int
	// StreamIdleTimeout 是单流接收缓冲被写满后允许的最长阻塞时间；
	// 超过则重置该流（RST）以保护整条会话（dev.md §3.7）。默认 600s。
	StreamIdleTimeout time.Duration
	// MaxStreams 是单会话流数上限，默认 256。
	MaxStreams int
	// MaxDataPayload 是单帧 DATA 的 Payload 上限，默认 protocol.MaxDataPayload。
	MaxDataPayload int
	// MaxFrameBody 是接收侧单帧帧体上限，默认 protocol.MaxFrameBody。
	MaxFrameBody int
	// Metrics 是进程级计数器；为 nil 时使用 metrics.Default。
	Metrics *metrics.Registry
	Logger  *slog.Logger
	// Now 与 Rand 可为空，测试时可注入。
	Now  func() time.Time
	Rand io.Reader
}

func (o *Options) normalize() error {
	if len(o.PSK) != protocol.PSKSize {
		return fmt.Errorf("%w: 需要 %d 字节，实际 %d", ErrNoPSK, protocol.PSKSize, len(o.PSK))
	}
	if o.AEAD == "" {
		o.AEAD = protocol.AEADChaCha20Poly1305
	}
	if !protocol.ValidAEAD(o.AEAD) {
		return fmt.Errorf("%w: %q", protocol.ErrUnknownAEAD, o.AEAD)
	}
	if o.TokenID == 0 {
		o.TokenID = protocol.DefaultTokenID
	}
	if o.AuthTimeout <= 0 {
		o.AuthTimeout = 5 * time.Second
	}
	if o.HeartbeatMiss <= 0 {
		o.HeartbeatMiss = 3
	}
	if o.WriteQueue <= 0 {
		o.WriteQueue = 1024
	}
	if o.StreamIdleTimeout <= 0 {
		o.StreamIdleTimeout = 600 * time.Second
	}
	if o.MaxStreams <= 0 {
		o.MaxStreams = 256
	}
	if o.MaxDataPayload <= 0 || o.MaxDataPayload > protocol.MaxFrameBody {
		o.MaxDataPayload = protocol.MaxDataPayload
	}
	if o.MaxFrameBody <= 0 || o.MaxFrameBody > protocol.MaxFrameBody {
		o.MaxFrameBody = protocol.MaxFrameBody
	}
	if o.Metrics == nil {
		o.Metrics = metrics.Default
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = cryptorand.Reader
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Role == RoleServer {
		if o.TimestampWindow <= 0 {
			return fmt.Errorf("%w: timestamp_window 必须大于 0", protocol.ErrReplayCacheCfg)
		}
		if o.ReplayCache == nil {
			// 重放缓存必须跨会话共享，否则同一 nonce 换个会话就能重放。
			return ErrNoReplayCache
		}
	}
	return nil
}

// Stats 是会话运行统计（用于日志与指标）。
type Stats struct {
	Conv      uint32
	Role      Role
	Remote    string
	PingsSent uint64
	PongsRecv uint64
	FramesIn  uint64
	FramesOut uint64
	PongsMiss int
	Secure    bool
}

// Session 是一条已认证的 KCP 会话（多路复用器的载体）。
//
// 生命周期：AcceptSession / DialSession 完成握手 → start 启动三个 goroutine：
// readLoop（唯一读）、writeLoop（唯一写，单写原则）、heartbeatLoop（可选）。
type Session struct {
	conn net.Conn
	role Role
	opts Options
	log  *slog.Logger
	conv uint32

	readCipher  protocol.Cipher
	readNonce   *protocol.NonceCounter
	writeCipher protocol.Cipher
	writeNonce  *protocol.NonceCounter

	sendCh    chan protocol.Frame
	closed    chan struct{}
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error

	handlersMu sync.RWMutex
	handlers   map[protocol.Type]Handler

	// streams 是 StreamID → 流的映射；nextStreamID 由客户端侧单调递增分配（奇数）。
	streamsMu    sync.Mutex
	streams      map[uint32]*stream
	nextStreamID uint32

	hb *heartbeatState

	wg sync.WaitGroup

	pingsSent  atomic.Uint64
	pongsRecv  atomic.Uint64
	framesIn   atomic.Uint64
	framesOut  atomic.Uint64
	pingsReply atomic.Uint64
}

func newSession(conn net.Conn, opts Options) (*Session, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	conv := convOf(conn)
	s := &Session{
		conn:         conn,
		role:         opts.Role,
		opts:         opts,
		conv:         conv,
		sendCh:       make(chan protocol.Frame, opts.WriteQueue),
		closed:       make(chan struct{}),
		streams:      make(map[uint32]*stream),
		nextStreamID: 1,
		hb:           &heartbeatState{miss: opts.HeartbeatMiss},
	}
	s.log = log.WithSession(opts.Logger, conv, conn.RemoteAddr().String()).With("role", opts.Role.String())
	return s, nil
}

// convOf 尽力取 KCP 会话号（Conver），取不到时返回 0。
func convOf(c net.Conn) uint32 {
	if g, ok := c.(interface{ GetConv() uint32 }); ok {
		return g.GetConv()
	}
	return 0
}

// AcceptSession 以 Server 身份完成认证握手并启动会话。
func AcceptSession(ctx context.Context, conn net.Conn, opts Options) (*Session, error) {
	opts.Role = RoleServer
	s, err := newSession(conn, opts)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := s.serverHandshake(); err != nil {
		s.opts.Metrics.AuthFailures.Add(1)
		s.closeWith(err)
		return nil, err
	}
	s.start()
	return s, nil
}

// DialSession 以 Client 身份完成认证握手并启动会话。
func DialSession(ctx context.Context, conn net.Conn, opts Options) (*Session, error) {
	opts.Role = RoleClient
	s, err := newSession(conn, opts)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := s.clientHandshake(); err != nil {
		s.opts.Metrics.AuthFailures.Add(1)
		s.closeWith(err)
		return nil, err
	}
	s.start()
	return s, nil
}

// withAuthDeadline 在握手期间设置连接级 deadline，结束后清除。
func (s *Session) withAuthDeadline(fn func() error) error {
	if err := s.conn.SetDeadline(s.opts.Now().Add(s.opts.AuthTimeout)); err != nil {
		return fmt.Errorf("mux: 设置握手超时: %w", err)
	}
	defer func() { _ = s.conn.SetDeadline(time.Time{}) }()

	err := fn()
	if err == nil {
		return nil
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("%w: %v", ErrAuthTimeout, err)
	}
	return err
}

func (s *Session) clientHandshake() error {
	init, err := protocol.NewInitiator(protocol.HandshakeConfig{
		PSK:     s.opts.PSK,
		AEAD:    s.opts.AEAD,
		TokenID: s.opts.TokenID,
		Now:     s.opts.Now,
		Rand:    s.opts.Rand,
	})
	if err != nil {
		return err
	}
	req, err := init.NewRequest()
	if err != nil {
		return err
	}
	raw, err := protocol.MarshalPlain(protocol.Frame{Type: protocol.TypeAuthReq, Payload: req.Marshal()})
	if err != nil {
		return err
	}

	var resp protocol.AuthResponse
	err = s.withAuthDeadline(func() error {
		if _, err := s.conn.Write(raw); err != nil {
			return fmt.Errorf("mux: 发送 AUTH_REQ: %w", err)
		}
		hdr, body, err := protocol.ReadFrame(s.conn, uint32(s.opts.MaxFrameBody))
		if err != nil {
			return fmt.Errorf("mux: 读取 AUTH_RESP: %w", err)
		}
		f, err := protocol.UnmarshalPlainBody(hdr, body)
		if err != nil {
			return fmt.Errorf("mux: 解析 AUTH_RESP: %w", err)
		}
		if f.Type != protocol.TypeAuthResp {
			return fmt.Errorf("%w: 期望 AUTH_RESP，实际 %s", ErrUnexpectedFrame, f.Type)
		}
		resp, err = protocol.ParseAuthResponse(f.Payload)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	keys, err := init.Complete(req, resp)
	if err != nil {
		return err
	}
	s.setKeys(keys, RoleClient)
	return nil
}

func (s *Session) serverHandshake() error {
	auth, err := protocol.NewAuthenticator(protocol.AuthenticatorConfig{
		HandshakeConfig: protocol.HandshakeConfig{
			PSK:     s.opts.PSK,
			AEAD:    s.opts.AEAD,
			TokenID: s.opts.TokenID,
			Now:     s.opts.Now,
			Rand:    s.opts.Rand,
		},
		Window: s.opts.TimestampWindow,
		Replay: s.opts.ReplayCache,
	})
	if err != nil {
		return err
	}

	var (
		req  protocol.AuthRequest
		resp protocol.AuthResponse
	)
	err = s.withAuthDeadline(func() error {
		hdr, body, err := protocol.ReadFrame(s.conn, uint32(s.opts.MaxFrameBody))
		if err != nil {
			return fmt.Errorf("mux: 读取 AUTH_REQ: %w", err)
		}
		f, err := protocol.UnmarshalPlainBody(hdr, body)
		if err != nil {
			return fmt.Errorf("mux: 解析 AUTH_REQ: %w", err)
		}
		if f.Type != protocol.TypeAuthReq {
			return fmt.Errorf("%w: 期望 AUTH_REQ，实际 %s", ErrUnexpectedFrame, f.Type)
		}
		req, err = protocol.ParseAuthRequest(f.Payload)
		if err != nil {
			return err
		}
		resp, err = auth.Verify(req)
		if err != nil {
			return err
		}
		// 无论成败都要把结论回给对方（失败时随后关闭）。
		out, err := protocol.MarshalPlain(protocol.Frame{Type: protocol.TypeAuthResp, Payload: resp.Marshal()})
		if err != nil {
			return err
		}
		if _, err := s.conn.Write(out); err != nil {
			return fmt.Errorf("mux: 发送 AUTH_RESP: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if resp.Code != protocol.AuthCodeOK {
		return fmt.Errorf("%w: code=%s", protocol.ErrAuthFailed, protocol.AuthCodeString(resp.Code))
	}
	keys, err := auth.Complete(req, resp)
	if err != nil {
		return err
	}
	s.setKeys(keys, RoleServer)
	return nil
}

// setKeys 安装双向密钥与独立的 nonce 计数器。
func (s *Session) setKeys(keys protocol.DirectionKeys, role Role) {
	if role == RoleClient {
		s.writeCipher, s.writeNonce = keys.C2S, protocol.NewNonceCounter()
		s.readCipher, s.readNonce = keys.S2C, protocol.NewNonceCounter()
		return
	}
	s.readCipher, s.readNonce = keys.C2S, protocol.NewNonceCounter()
	s.writeCipher, s.writeNonce = keys.S2C, protocol.NewNonceCounter()
}

func (s *Session) start() {
	s.log.Info("会话认证完成，进入安全态",
		log.Event, "session_up",
		"aead", s.opts.AEAD,
		"heartbeat_interval", s.opts.HeartbeatInterval.String(),
	)
	s.opts.Metrics.SessionsActive.Add(1)
	s.opts.Metrics.SessionsTotal.Add(1)
	s.wg.Add(2)
	go s.readLoop()
	go s.writeLoop()
	if s.opts.HeartbeatInterval > 0 {
		s.wg.Add(1)
		go s.heartbeatLoop()
	}
}

// readLoop 是唯一的读 goroutine。
func (s *Session) readLoop() {
	defer s.wg.Done()
	for {
		hdr, body, err := protocol.ReadFrame(s.conn, uint32(s.opts.MaxFrameBody))
		if err != nil {
			s.closeWith(fmt.Errorf("mux: 读取帧失败: %w", err))
			return
		}
		f, err := protocol.UnmarshalSecure(s.readCipher, s.readNonce, hdr, body)
		if err != nil {
			// 不降级：安全态解密/完整性失败一律断开。
			s.closeWith(err)
			return
		}
		s.framesIn.Add(1)
		s.dispatch(f)
	}
}

// dispatch 分发一帧。
//
// 流数据帧（DATA/FIN/RST）与 PING/PONG/GOAWAY/ERROR 由 Session 直接处理，
// 其余类型（如 CONNECT_REQ/CONNECT_RESP）交给应用注册的 Handler。
func (s *Session) dispatch(f protocol.Frame) {
	switch f.Type {
	case protocol.TypeData:
		s.deliverData(f)
		return
	case protocol.TypeFin:
		s.deliverFin(f)
		return
	case protocol.TypeRst:
		s.deliverRst(f)
		return
	case protocol.TypePing:
		// 回显 payload，便于对端校验（dev.md §3.9）。
		ctx, cancel := context.WithTimeout(context.Background(), heartbeatSendTimeout)
		defer cancel()
		if err := s.SendControl(ctx, protocol.TypePong, f.Payload); err != nil {
			s.log.Debug("回复 PONG 失败", "err", err)
			return
		}
		s.pingsReply.Add(1)
		return
	case protocol.TypePong:
		s.pongsRecv.Add(1)
		s.hb.onPong()
		return
	case protocol.TypeGoAway:
		s.log.Info("收到 GOAWAY", log.Event, "goaway")
		s.closeWith(ErrSessionClosed)
		return
	case protocol.TypeError:
		s.log.Warn("收到 ERROR 帧", log.Event, "error_frame", "payload", string(f.Payload))
		return
	}

	s.handlersMu.RLock()
	h := s.handlers[f.Type]
	s.handlersMu.RUnlock()
	if h != nil {
		h(s, f)
		return
	}
	s.log.Warn("收到未处理的帧类型", log.Event, "unhandled_frame", "type", f.Type.String())
}

// writeLoop 是唯一的写 goroutine（单写原则，dev.md §11.1）。
func (s *Session) writeLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.closed:
			return
		case f := <-s.sendCh:
			raw, err := protocol.MarshalSecure(s.writeCipher, s.writeNonce, f)
			if err != nil {
				s.closeWith(fmt.Errorf("mux: 编码帧失败: %w", err))
				return
			}
			if _, err := s.conn.Write(raw); err != nil {
				s.closeWith(fmt.Errorf("mux: 写入帧失败: %w", err))
				return
			}
			s.framesOut.Add(1)
		}
	}
}

// heartbeatLoop 以抖动间隔发送 PING，连续 miss 次未收到 PONG 即判定失效。
func (s *Session) heartbeatLoop() {
	defer s.wg.Done()
	timer := time.NewTimer(jitter(s.opts.HeartbeatInterval))
	defer timer.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-timer.C:
			if s.hb.tick(s) {
				s.closeWith(ErrHeartbeatTimeout)
				return
			}
			timer.Reset(jitter(s.opts.HeartbeatInterval))
		}
	}
}

// jitter 在 ±25% 内抖动，避免形成周期性小包指纹（dev.md §4.4）。
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	quarter := int64(d) / 4
	if quarter <= 0 {
		return d
	}
	delta := rand.Int63n(2*quarter+1) - quarter
	out := d + time.Duration(delta)
	if out <= 0 {
		return d
	}
	return out
}

// heartbeatState 维护心跳的等待与miss计数。
type heartbeatState struct {
	mu       sync.Mutex
	miss     int
	awaiting bool
	misses   int
}

// tick 返回 true 表示应判定会话失效。
func (h *heartbeatState) tick(s *Session) bool {
	h.mu.Lock()
	if h.awaiting {
		h.misses++
		if h.misses >= h.miss {
			h.mu.Unlock()
			return true
		}
	}
	payload := make([]byte, 8)
	if _, err := io.ReadFull(s.opts.Rand, payload); err != nil {
		binary.BigEndian.PutUint64(payload, uint64(s.opts.Now().UnixNano()))
	}
	h.awaiting = true
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), heartbeatSendTimeout)
	defer cancel()
	if err := s.SendControl(ctx, protocol.TypePing, payload); err != nil {
		s.log.Debug("发送 PING 失败", "err", err)
		return false
	}
	s.pingsSent.Add(1)
	return false
}

func (h *heartbeatState) onPong() {
	h.mu.Lock()
	h.awaiting = false
	h.misses = 0
	h.mu.Unlock()
}

func (h *heartbeatState) missCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.misses
}

// SendControl 发送一个 Session 级控制帧（StreamID=0）。payload 会被复制。
func (s *Session) SendControl(ctx context.Context, t protocol.Type, payload []byte) error {
	return s.send(ctx, protocol.Frame{Type: t, StreamID: 0, Payload: payload})
}

// send 把帧放入发送队列；队列满时按背压语义阻塞到 ctx 结束或会话关闭。
func (s *Session) send(ctx context.Context, f protocol.Frame) error {
	if len(f.Payload) > protocol.MaxFrameBody {
		return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(f.Payload), protocol.MaxFrameBody)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	f.Payload = append([]byte(nil), f.Payload...)
	// 先做一次关闭预检：否则当 closed 与 sendCh 同时可写时，select 会随机命中后者，
	// 使「会话关闭后发送」偶发成功。
	select {
	case <-s.closed:
		return s.closeErr()
	default:
	}
	select {
	case <-s.closed:
		return s.closeErr()
	case s.sendCh <- f:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Handle 注册某类型帧的处理器。
func (s *Session) Handle(t protocol.Type, h Handler) {
	s.handlersMu.Lock()
	defer s.handlersMu.Unlock()
	if s.handlers == nil {
		s.handlers = make(map[protocol.Type]Handler)
	}
	s.handlers[t] = h
}

// Close 主动关闭会话。
func (s *Session) Close() error {
	s.closeWith(ErrSessionClosed)
	return nil
}

// closeWith 幂等地关闭会话并记录原因。
func (s *Session) closeWith(err error) {
	s.closeOnce.Do(func() {
		s.errMu.Lock()
		s.err = err
		s.errMu.Unlock()
		close(s.closed)
		_ = s.conn.Close()
		s.opts.Metrics.SessionsActive.Add(-1)
		// 会话关闭等于向所有流广播 RST（dev.md §3.8）。
		s.resetAllStreams(ErrSessionClosed)
		s.log.Info("会话关闭",
			log.Event, "session_down",
			"err", err,
			"frames_in", s.framesIn.Load(),
			"frames_out", s.framesOut.Load(),
			"pings_sent", s.pingsSent.Load(),
			"pongs_recv", s.pongsRecv.Load(),
		)
	})
}

// Done 在会话关闭后关闭。
func (s *Session) Done() <-chan struct{} { return s.closed }

// Wait 阻塞直到会话关闭并返回关闭原因。
func (s *Session) Wait() error {
	<-s.closed
	return s.closeErr()
}

// closeErr 返回关闭原因（未设置时视为主动关闭）。
func (s *Session) closeErr() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		return ErrSessionClosed
	}
	return s.err
}

// Conv 返回 KCP 会话号。
func (s *Session) Conv() uint32 { return s.conv }

// Role 返回会话角色。
func (s *Session) Role() Role { return s.role }

// RemoteAddr 返回对端地址。
func (s *Session) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }

// PingsSent 返回已发送的 PING 数。
func (s *Session) PingsSent() uint64 { return s.pingsSent.Load() }

// PongsReceived 返回已收到的 PONG 数。
func (s *Session) PongsReceived() uint64 { return s.pongsRecv.Load() }

// Stats 返回会话统计快照。
func (s *Session) Stats() Stats {
	return Stats{
		Conv:      s.conv,
		Role:      s.role,
		Remote:    s.conn.RemoteAddr().String(),
		PingsSent: s.pingsSent.Load(),
		PongsRecv: s.pongsRecv.Load(),
		FramesIn:  s.framesIn.Load(),
		FramesOut: s.framesOut.Load(),
		PongsMiss: s.hb.missCount(),
		Secure:    s.readCipher != nil && s.writeCipher != nil,
	}
}
