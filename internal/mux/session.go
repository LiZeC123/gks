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
	// ErrSessionSilence 表示对端长时间没有任何入站帧，判定会话失效（被动判活）。
	ErrSessionSilence = errors.New("mux: 对端沉默超时，会话失效")
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
	// PeerSilenceTimeout > 0 时启用「被动判活」：连续这么久没有收到任何完整入站帧，就判定
	// 对端已经消失并回收会话（不发送任何额外流量）。0 表示关闭。
	//
	// 服务端用它兜住「客户端进程消失、断电、网络分区」等连 GOAWAY 都发不出来的情形；客户端的
	// 判活由自己的心跳承担，因此不必启用。阈值口径见 config.Heartbeat.SilenceTimeout()。
	PeerSilenceTimeout time.Duration
	// WriteQueue 是发送队列长度，默认 1024。
	WriteQueue int
	// StreamStallTimeout 是单流接收缓冲被写满后允许的最长阻塞时间；
	// 超过则重置该流（RST）以保护整条会话（dev.md §3.7）。默认 600s。
	StreamStallTimeout time.Duration
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
	if o.StreamStallTimeout <= 0 {
		o.StreamStallTimeout = 600 * time.Second
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

	sendCh    chan outFrame
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

	// t0 是会话建立时刻；lastRecv 是「距 t0 的纳秒数」，在收到任何完整入站帧时刷新。
	// 两者都用 time.Since 的单调读数，避免系统时钟跳变导致误判沉默。
	t0       time.Time
	lastRecv atomic.Int64

	// drainDone 在优雅关闭时创建，底层连接真正释放后关闭（见 WaitDrained）。
	drainMu   sync.Mutex
	drainDone chan struct{}

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
		sendCh:       make(chan outFrame, opts.WriteQueue),
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
	// 被动判活的计时从会话进入安全态开始。
	s.t0 = time.Now()
	s.wg.Add(2)
	go s.readLoop()
	go s.writeLoop()
	if s.opts.HeartbeatInterval > 0 {
		s.wg.Add(1)
		go s.heartbeatLoop()
	}
	if s.opts.PeerSilenceTimeout > 0 {
		s.wg.Add(1)
		go s.silenceLoop()
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
		// 任何完整入站帧都算「对端还活着」（不只是 PING）：主动判活的会话里，DATA 与心跳
		// 都可能先到，只认 PING 会在对端不心跳时误杀。
		if s.opts.PeerSilenceTimeout > 0 {
			s.lastRecv.Store(int64(time.Since(s.t0)))
		}
		s.dispatch(f)
	}
}

// silenceLoop 是被动判活：连续 PeerSilenceTimeout 没有收到任何入站帧就回收会话。
//
// 与 heartbeatLoop 的区别：本循环不发送任何帧，只是观察对端是否还在说话，因此不改变服务端的
// 流量特征。检测粒度由 silenceCheckInterval 决定，实际回收时间 = PeerSilenceTimeout + 一个检查周期。
func (s *Session) silenceLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.silenceCheckInterval())
	defer ticker.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-ticker.C:
			silent := time.Since(s.t0) - time.Duration(s.lastRecv.Load())
			if silent <= s.opts.PeerSilenceTimeout {
				continue
			}
			s.log.Warn("对端沉默超时，回收会话",
				log.Event, "session_silence",
				"silent", silent.String(),
				"timeout", s.opts.PeerSilenceTimeout.String(),
				"frames_in", s.framesIn.Load(),
			)
			s.closeWith(ErrSessionSilence)
			return
		}
	}
}

// silenceCheckInterval 由阈值推导检查周期：取 1/4 并限制在 [50ms, 5s]。
//
// 上界 5s 让长时间阈值（默认 100s）也不会因为检查过密而空转；下界保证测试能用小阈值观察。
func (s *Session) silenceCheckInterval() time.Duration {
	d := s.opts.PeerSilenceTimeout / 4
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	if d < 50*time.Millisecond {
		d = 50 * time.Millisecond
	}
	return d
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
	// 退出时唤醒仍在等待写出结果的调用方（sendSync），否则它们只能等 ctx 超时。
	defer s.failPending()
	for {
		select {
		case <-s.closed:
			return
		case of := <-s.sendCh:
			raw, err := protocol.MarshalSecure(s.writeCipher, s.writeNonce, of.f)
			if err != nil {
				err = fmt.Errorf("mux: 编码帧失败: %w", err)
				signalDone(of.done, err)
				s.closeWith(err)
				return
			}
			if _, err := s.conn.Write(raw); err != nil {
				err = fmt.Errorf("mux: 写入帧失败: %w", err)
				signalDone(of.done, err)
				s.closeWith(err)
				return
			}
			s.framesOut.Add(1)
			signalDone(of.done, nil)
		}
	}
}

// failPending 把队列里剩余出站单元的等待者全部唤醒并报告会话已关闭。
func (s *Session) failPending() {
	for {
		select {
		case of := <-s.sendCh:
			signalDone(of.done, s.closeErr())
		default:
			return
		}
	}
}

// signalDone 非阻塞地投递写出结果；done 为 nil 表示调用方不等待（普通发送路径）。
func signalDone(done chan error, err error) {
	if done == nil {
		return
	}
	select {
	case done <- err:
	default:
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

// outFrame 是一个待写出的帧。done 非 nil 时，writeLoop 写出后会投递结果（缓冲 1，不会泄漏）。
type outFrame struct {
	f    protocol.Frame
	done chan error
}

// send 把帧放入发送队列；队列满时按背压语义阻塞到 ctx 结束或会话关闭。
func (s *Session) send(ctx context.Context, f protocol.Frame) error {
	return s.enqueue(ctx, outFrame{f: f})
}

// sendSync 入队并等待该帧真正写出（或失败/会话关闭/ctx 结束）。
//
// 用于「必须确认已离开本机」的帧（当前只有 GOAWAY）。注意：
//   - 返回 nil 只表示这一帧已交给底层 conn.Write，不表示对端已收到（协议没有 ACK）；
//   - 绝不能在 writeLoop 内部调用，否则会自等自死锁。
func (s *Session) sendSync(ctx context.Context, f protocol.Frame) error {
	done := make(chan error, 1)
	if err := s.enqueue(ctx, outFrame{f: f, done: done}); err != nil {
		return err
	}
	// 先看结果是否已经就绪：写完与会话关闭可能同时发生，避免误报为失败。
	select {
	case err := <-done:
		return err
	default:
	}
	select {
	case err := <-done:
		return err
	case <-s.closed:
		return s.closeErr()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// enqueue 把出站单元放入发送队列；队列满时按背压语义阻塞到 ctx 结束或会话关闭。
func (s *Session) enqueue(ctx context.Context, of outFrame) error {
	if len(of.f.Payload) > protocol.MaxFrameBody {
		return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(of.f.Payload), protocol.MaxFrameBody)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	of.f.Payload = append([]byte(nil), of.f.Payload...)
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
	case s.sendCh <- of:
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

// Close 主动关闭会话（不通知对端）。
//
// 适用于「对端已经不可用」的路径（读写出错、会话失效、丢弃坏会话）；优雅放弃一条仍然
// 健康的会话请用 CloseWithGoAway，否则对端只能靠沉默判死才回收它。
func (s *Session) Close() error {
	s.closeWith(ErrSessionClosed)
	return nil
}

// CloseWithGoAway 先发 GOAWAY 并等待它写出，再关闭会话。
//
// 用于主动且优雅地放弃一条会话（客户端池回收空闲会话、服务端退出等）：对端收到 GOAWAY 后
// 立刻回收该会话及其上的流，不必等到沉默判死。ctx 是等待写出的上限——对端不可达时会等到
// ctx 结束，但关闭一定会发生（本函数不会因为写出失败而把会话留着）。
//
// 为什么需要「等待写出」：帧只是入队，closeWith 会立刻关闭底层 conn，writeLoop 的 select
// 在 closed 与 sendCh 同时就绪时随机选择，直接 Close 会让 GOAWAY 静默丢失。
//
// 返回值只表示本帧的写出结果：返回 nil 即已交给底层；返回非 nil 时无法区分「没写出去」与
// 「写出去之后对端立刻关闭了连接（本端读循环同时记录了 socket 错误）」——对端收到 GOAWAY
// 就会立刻关闭，这是正常行为，调用方不应据此判定失败。
func (s *Session) CloseWithGoAway(ctx context.Context) error {
	err := s.sendSync(ctx, protocol.Frame{Type: protocol.TypeGoAway})
	s.closeWithGracefulConn(ErrSessionClosed)
	return err
}

// CloseWithGoAwayAndDrain 与 CloseWithGoAway 相同，并等到底层连接真正释放后才返回。
//
// 供进程退出流程使用：kcp-go 的写出是异步的（见 gracefulDrainer），而监听 socket 被所有会话
// 共用——关闭监听之前必须确认最后一批 GOAWAY 已经真正发出去，否则它们会被一起丢掉。
// 客户端热路径不要用它（会引入一个排空窗口的延迟）。
func (s *Session) CloseWithGoAwayAndDrain(ctx context.Context) error {
	err := s.CloseWithGoAway(ctx)
	if waitErr := s.WaitDrained(ctx); waitErr != nil {
		return waitErr
	}
	return err
}

// WaitDrained 等待底层连接真正释放；若该会话没有走优雅关闭（无排空可等）则立即返回。
func (s *Session) WaitDrained(ctx context.Context) error {
	s.drainMu.Lock()
	done := s.drainDone
	s.drainMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// closeWith 幂等地关闭会话并记录原因。
func (s *Session) closeWith(err error) {
	s.shutdown(err, false)
}

// closeWithGracefulConn 与 closeWith 相同，但底层连接晚一点释放：给传输层把「已写出但尚未
// 真正发到网络」的数据送出去的机会（见 gracefulDrainer）。会话本身立即对所有调用方可见为已关闭。
func (s *Session) closeWithGracefulConn(err error) {
	s.shutdown(err, true)
}

// shutdown 幂等地关闭会话并记录原因；drainConn 决定底层连接是立即关闭还是延后释放。
func (s *Session) shutdown(err error, drainConn bool) {
	s.closeOnce.Do(func() {
		s.errMu.Lock()
		s.err = err
		s.errMu.Unlock()
		close(s.closed)
		if drainConn {
			done := make(chan struct{})
			s.drainMu.Lock()
			s.drainDone = done
			s.drainMu.Unlock()
			go s.closeConnAfterDrain(done)
		} else {
			_ = s.conn.Close()
		}
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

// gracefulDrainer 由传输层可选实现：关闭底层连接前，先给「已交给传输层但尚未真正发出」的数据
// 一点时间落到网络上。接口定义在消费方（mux），因此 mux 不依赖具体传输实现。
//
// 为什么需要它：kcp-go 的 UDPSession.Write 只是把数据交给它的 per-session 后处理 goroutine
// （FEC → 加密 → 批量发送），紧接着 Close 会关闭底层 UDP socket，未发出的数据报被直接丢弃——
// GOAWAY 就是典型受害者。实测：写出后立刻 Close，对端读不到该帧；等待 100ms 后连续 5 次都读到。
type gracefulDrainer interface {
	// CloseAfterDrain 最多等待 d，然后关闭底层连接。
	CloseAfterDrain(d time.Duration) error
}

// goAwayDrainDelay 是优雅关闭时留给传输层发送的宽限时间。
//
// 依据：kcp-go 的后处理 goroutine 收到数据即处理、没有固定定时器，100ms 对「发出一个小包」极其
// 充裕；而释放是异步的（见 closeConnAfterDrain），真正代价只是 socket 多存活 100ms。
const goAwayDrainDelay = 100 * time.Millisecond

// closeConnAfterDrain 延后释放底层连接，并在真正释放后关闭 done。
//
// 不能阻塞当前调用方（池回收、连接收尾都在热路径上），因此由 shutdown 放到独立 goroutine：
// 会话已经关闭，表项、流与指标都已回收，这里只是让 socket 多活一小会儿。
func (s *Session) closeConnAfterDrain(done chan struct{}) {
	defer close(done)
	dc, ok := s.conn.(gracefulDrainer)
	if !ok {
		_ = s.conn.Close()
		return
	}
	_ = dc.CloseAfterDrain(goAwayDrainDelay)
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
