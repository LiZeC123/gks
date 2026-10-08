package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/LiZeC123/gks/internal/log"
	"github.com/LiZeC123/gks/internal/mux"
	"github.com/LiZeC123/gks/internal/protocol"
)

// Dialer 建立到服务端的传输层连接（transport.Dialer 满足该接口）。
type Dialer interface {
	Dial(addr string) (net.Conn, error)
}

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
}

// Handler 处理单条本地 SOCKS5 连接。
//
// 阶段 3 的形态：每条 SOCKS5 连接建立一条独立 KCP Session（连接池见阶段 5），
// 会话上只开一条流（多路复用见阶段 4）。
type Handler struct {
	cfg    HandlerConfig
	dialer Dialer
	log    *slog.Logger
}

// NewHandler 构造 Handler。
func NewHandler(cfg HandlerConfig, dialer Dialer) *Handler {
	logger := cfg.SessionOptions.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{cfg: cfg, dialer: dialer, log: logger}
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
		if rep, ok := ReplyForError(err); ok {
			_ = WriteReply(conn, rep, protocol.UnspecificBind())
		}
		h.log.Warn("本地 SOCKS5 握手失败",
			log.Event, "socks5_failed", log.Remote, local, "err", err)
		return
	}

	sess, err := h.dialSession(ctx)
	if err != nil {
		_ = WriteReply(conn, protocol.RepHostUnreachable, protocol.UnspecificBind())
		h.log.Warn("建立会话失败",
			log.Event, "session_dial_failed", log.Remote, local,
			log.Target, target.String(), "err", err)
		return
	}
	defer func() { _ = sess.Close() }()

	stream, err := sess.OpenStream(ctx)
	if err != nil {
		_ = WriteReply(conn, protocol.RepGeneralFailure, protocol.UnspecificBind())
		h.log.Warn("开流失败", log.Event, "open_stream_failed", log.Remote, local, "err", err)
		return
	}
	defer func() { _ = stream.Close() }()

	resp, err := h.connectRemote(ctx, sess, stream, target)
	if err != nil {
		_ = WriteReply(conn, protocol.RepGeneralFailure, protocol.UnspecificBind())
		h.log.Warn("CONNECT_REQ 失败",
			log.Event, "connect_failed", log.Remote, local,
			log.Target, target.String(), "err", err)
		return
	}
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

// dialSession 建立到服务端的 KCP 会话并完成认证。
func (h *Handler) dialSession(ctx context.Context) (*mux.Session, error) {
	kconn, err := h.dialer.Dial(h.cfg.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("连接服务端 %s: %w", h.cfg.ServerAddr, err)
	}
	sess, err := mux.DialSession(ctx, kconn, h.cfg.SessionOptions)
	if err != nil {
		_ = kconn.Close()
		return nil, fmt.Errorf("会话认证失败: %w", err)
	}
	return sess, nil
}

// connectRemote 发送 CONNECT_REQ 并等待 CONNECT_RESP。
func (h *Handler) connectRemote(
	ctx context.Context,
	sess *mux.Session,
	stream mux.Stream,
	target Target,
) (protocol.ConnectResponse, error) {
	respCh := make(chan protocol.ConnectResponse, 1)
	sess.Handle(protocol.TypeConnectResp, func(_ *mux.Session, f protocol.Frame) {
		resp, err := protocol.ParseConnectResponse(f.Payload)
		if err != nil {
			return
		}
		select {
		case respCh <- resp:
		default:
		}
	})

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
	case resp := <-respCh:
		return resp, nil
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
