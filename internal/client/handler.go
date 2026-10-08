package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/LiZeC123/gks/internal/log"
	"github.com/LiZeC123/gks/internal/metrics"
	"github.com/LiZeC123/gks/internal/mux"
	"github.com/LiZeC123/gks/internal/protocol"
)

// Dialer 建立到服务端的传输层连接（transport.Dialer 满足该接口）。
type Dialer interface {
	Dial(addr string) (net.Conn, error)
}

// SessionSource 提供已认证的会话。生产路径用 *SessionPool；
// 单测可以传入任何实现（例如下面的 dialSessionSource）。
type SessionSource interface {
	Acquire(ctx context.Context) (*mux.Session, error)
	Release(sess *mux.Session)
	Discard(sess *mux.Session)
}

// dialSessionSource 是「每条连接新建一条会话」的退化实现（不池化）。
type dialSessionSource struct {
	dialer Dialer
	opts   mux.Options
	server string
}

func (s *dialSessionSource) Acquire(ctx context.Context) (*mux.Session, error) {
	conn, err := s.dialer.Dial(s.server)
	if err != nil {
		return nil, fmt.Errorf("连接服务端 %s: %w", s.server, err)
	}
	sess, err := mux.DialSession(ctx, conn, s.opts)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("会话认证失败: %w", err)
	}
	return sess, nil
}

func (s *dialSessionSource) Release(sess *mux.Session) { _ = sess.Close() }
func (s *dialSessionSource) Discard(sess *mux.Session) { _ = sess.Close() }

// HandlerConfig 是 Handler 的运行参数（由 cmd 从配置装配）。
type HandlerConfig struct {
	// ServerAddr 是服务端的 KCP/UDP 地址。
	ServerAddr string
	// SessionOptions 是 mux 会话参数（PSK/AEAD/心跳等）。
	SessionOptions mux.Options
	// HandshakeTimeout 覆盖本地 SOCKS5 协商与请求解析。
	HandshakeTimeout time.Duration
	// ConnectTimeout 覆盖等待服务端 CONNECT_RESP。
	ConnectTimeout time.Duration
	// BridgeBuffer 为 0 时使用默认值。
	BridgeBuffer int
	// Sessions 是会话来源；为 nil 时退化为「每条连接新建一条会话」。
	Sessions SessionSource
	// MaxConnectAttempts 是「会话中途失效」时的换会话重试次数，默认 3。
	MaxConnectAttempts int
}

// Handler 处理单条本地 SOCKS5 连接。
//
// 阶段 3 的形态：每条 SOCKS5 连接建立一条独立 KCP Session（连接池见阶段 5），
// 会话上只开一条流（多路复用见阶段 4）。
type Handler struct {
	cfg    HandlerConfig
	dialer Dialer
	log    *slog.Logger

	// routers 保证「一条会话只注册一个 CONNECT_RESP handler」，
	// 回包再按 StreamID 路由到具体连接（会话被池复用后必须如此）。
	routersMu sync.Mutex
	routers   map[*mux.Session]*connRouter
}

// NewHandler 构造 Handler。
func NewHandler(cfg HandlerConfig, dialer Dialer) *Handler {
	logger := cfg.SessionOptions.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{
		cfg:     cfg,
		dialer:  dialer,
		log:     logger,
		routers: make(map[*mux.Session]*connRouter),
	}
	if h.cfg.Sessions == nil {
		h.cfg.Sessions = &dialSessionSource{dialer: dialer, opts: cfg.SessionOptions, server: cfg.ServerAddr}
	}
	if h.cfg.MaxConnectAttempts <= 0 {
		h.cfg.MaxConnectAttempts = 3
	}
	// 池丢弃会话时同步清理路由表，避免按会话的资源泄漏。
	if pool, ok := h.cfg.Sessions.(*SessionPool); ok {
		pool.SetOnDrop(h.forgetSession)
	}
	return h
}

// routerFor 返回（必要时创建）该会话的路由器，并保证只注册一次 handler。
func (h *Handler) routerFor(sess *mux.Session) *connRouter {
	h.routersMu.Lock()
	defer h.routersMu.Unlock()
	if r, ok := h.routers[sess]; ok {
		return r
	}
	r := newConnRouter()
	sess.Handle(protocol.TypeConnectResp, r.dispatch)
	h.routers[sess] = r
	return r
}

// forgetSession 清理某条会话的路由表（会话被丢弃/回收时调用）。
func (h *Handler) forgetSession(sess *mux.Session) {
	if sess == nil {
		return
	}
	h.routersMu.Lock()
	r := h.routers[sess]
	delete(h.routers, sess)
	h.routersMu.Unlock()
	if r != nil {
		r.close()
	}
}

// Handle 处理一条本地 SOCKS5 连接。
func (h *Handler) Handle(ctx context.Context, conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			// panic 隔离：单条连接的问题不得影响进程（dev.md §11.6）。
			h.log.Error("连接处理 panic",
				log.Event, "panic",
				log.Remote, conn.RemoteAddr().String(),
				"panic", fmt.Sprint(r),
			)
		}
	}()
	defer func() { _ = conn.Close() }()

	local := conn.RemoteAddr().String()

	target, err := h.socks5Handshake(conn)
	if err != nil {
		metrics.Default.Socks5Failures.Add(1)
		if rep, ok := ReplyForError(err); ok {
			_ = WriteReply(conn, rep, protocol.UnspecificBind())
		}
		h.log.Warn("本地 SOCKS5 握手失败",
			log.Event, "socks5_failed", log.Remote, local, "err", err)
		return
	}

	sess, stream, resp, err := h.dialAndConnect(ctx, target)
	if err != nil {
		metrics.Default.SessionDialFailures.Add(1)
		_ = WriteReply(conn, protocol.RepHostUnreachable, protocol.UnspecificBind())
		h.log.Warn("建立会话或发起 CONNECT 失败",
			log.Event, "session_dial_failed", log.Remote, local,
			log.Target, target.String(), "err", err)
		return
	}
	defer h.releaseSession(sess)
	defer func() { _ = stream.Close() }()

	if resp.Reply != protocol.RepSuccess {
		_ = WriteReply(conn, resp.Reply, protocol.UnspecificBind())
		h.log.Warn("服务端拒绝连接",
			log.Event, "connect_rejected", log.Remote, local,
			log.Target, target.String(), "reply", protocol.RepString(resp.Reply))
		return
	}

	if err := WriteReply(conn, protocol.RepSuccess, resp.Bind); err != nil {
		h.log.Debug("回复 SOCKS5 成功失败", "err", err)
		return
	}
	h.log.Info("代理连接建立",
		log.Event, "proxy_up",
		log.Remote, local,
		log.Target, target.String(),
		"bind", resp.Bind.String(),
		log.SessionID, sess.Conv(),
		log.StreamID, stream.StreamID(),
	)

	if err := mux.Bridge(ctx, conn, stream, h.cfg.BridgeBuffer); err != nil && !isBenign(err) {
		h.log.Debug("转发结束", log.Event, "proxy_bridge_end", "err", err)
	}
	h.log.Info("代理连接关闭",
		log.Event, "proxy_down",
		log.Remote, local,
		log.Target, target.String(),
	)
}

// socks5Handshake 完成本地 SOCKS5 协商与请求解析（带超时）。
func (h *Handler) socks5Handshake(conn net.Conn) (Target, error) {
	if err := conn.SetDeadline(time.Now().Add(h.cfg.HandshakeTimeout)); err != nil {
		return Target{}, err
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	if err := Negotiate(conn); err != nil {
		return Target{}, err
	}
	return ReadRequest(conn)
}

// dialAndConnect 取会话 → 开流 → CONNECT_REQ/RESP。
//
// 只有当失败原因确实是「会话在过程中失效」时才换会话重试；服务端可能已经建好
// 目标连接的失败（例如等待回包超时）不重试——重试会在服务端多建一条连接。
func (h *Handler) dialAndConnect(
	ctx context.Context,
	target Target,
) (*mux.Session, mux.Stream, protocol.ConnectResponse, error) {
	var lastErr error
	for attempt := 1; attempt <= h.cfg.MaxConnectAttempts; attempt++ {
		sess, err := h.cfg.Sessions.Acquire(ctx)
		if err != nil {
			return nil, nil, protocol.ConnectResponse{}, err
		}
		stream, err := sess.OpenStream(ctx)
		if err != nil {
			h.discardSession(sess)
			lastErr = err
			continue
		}
		resp, err := h.connectRemote(ctx, sess, stream, target)
		if err == nil {
			return sess, stream, resp, nil
		}
		_ = stream.Close()
		dead := h.isSessionDead(sess, err)
		h.discardSession(sess)
		if !dead {
			return nil, nil, protocol.ConnectResponse{}, err
		}
		h.log.Warn("会话在连接过程中失效，换一条会话重试",
			log.Event, "session_retry", "attempt", attempt, "err", err)
		lastErr = err
	}
	if lastErr == nil {
		lastErr = mux.ErrSessionClosed
	}
	return nil, nil, protocol.ConnectResponse{}, fmt.Errorf("重试 %d 次仍失败: %w", h.cfg.MaxConnectAttempts, lastErr)
}

// isSessionDead 判断错误是否意味着「这条会话已经不可用」。
func (h *Handler) isSessionDead(sess *mux.Session, err error) bool {
	select {
	case <-sess.Done():
		return true
	default:
	}
	return errors.Is(err, ErrRouterClosed) ||
		errors.Is(err, mux.ErrSessionClosed) ||
		errors.Is(err, net.ErrClosed)
}

// releaseSession 归还会话：池化时交给池复用，否则关闭并清理路由表。
func (h *Handler) releaseSession(sess *mux.Session) {
	if sess == nil {
		return
	}
	if pool, ok := h.cfg.Sessions.(*SessionPool); ok {
		pool.Release(sess)
		return
	}
	h.forgetSession(sess)
	_ = sess.Close()
}

// discardSession 丢弃不可用的会话。
func (h *Handler) discardSession(sess *mux.Session) {
	if sess == nil {
		return
	}
	if pool, ok := h.cfg.Sessions.(*SessionPool); ok {
		pool.Discard(sess)
		return
	}
	h.forgetSession(sess)
	_ = sess.Close()
}

// connectRemote 发送 CONNECT_REQ 并等待属于该流的 CONNECT_RESP。
func (h *Handler) connectRemote(
	ctx context.Context,
	sess *mux.Session,
	stream mux.Stream,
	target Target,
) (protocol.ConnectResponse, error) {
	router := h.routerFor(sess)
	respCh := router.register(stream.StreamID())
	defer router.unregister(stream.StreamID())

	payload, err := protocol.ConnectRequest{Address: target}.Marshal()
	if err != nil {
		return protocol.ConnectResponse{}, err
	}
	if err := sess.SendFrame(ctx, protocol.TypeConnectReq, stream.StreamID(), payload); err != nil {
		return protocol.ConnectResponse{}, err
	}

	timer := time.NewTimer(h.cfg.ConnectTimeout)
	defer timer.Stop()
	select {
	case res := <-respCh:
		if res.err != nil {
			return protocol.ConnectResponse{}, res.err
		}
		return res.resp, nil
	case <-timer.C:
		return protocol.ConnectResponse{Reply: protocol.RepTTLExpired, Bind: protocol.UnspecificBind()}, nil
	case <-sess.Done():
		return protocol.ConnectResponse{}, fmt.Errorf("会话已关闭: %w", sess.Wait())
	}
}

// isBenign 判断转发结束是否属于正常的连接收尾。
func isBenign(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, mux.ErrSessionClosed) ||
		errors.Is(err, mux.ErrStreamReset) ||
		errors.Is(err, mux.ErrStreamWriteClosed)
}
