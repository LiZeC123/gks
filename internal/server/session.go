// Package server 实现 gks 服务端：接受 KCP 会话，并按 CONNECT_REQ 拨号转发。
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/LiZeC123/gks/internal/log"
	"github.com/LiZeC123/gks/internal/mux"
	"github.com/LiZeC123/gks/internal/protocol"
)

// goAwayFlushDelay 是发送 GOAWAY 后留给写循环的冲刷时间。
//
// 阶段 2/3 的简化实现：真正的「等待活跃流排空」需要先解决 kcp-go
// `Listener.Close()` 关闭共享 UDP socket 的问题（dev.md §15 第 22 条）。
const goAwayFlushDelay = 200 * time.Millisecond

// HandlerConfig 是 Handler 的运行参数（由 cmd 从配置装配）。
type HandlerConfig struct {
	// SessionOptions 是 mux 会话参数（PSK/AEAD/认证窗口/重放缓存等）。
	SessionOptions mux.Options
	// DialTimeout 是拨号目标的超时。
	DialTimeout time.Duration
	// Keepalive 是目标连接的 TCP keepalive 间隔。
	Keepalive time.Duration
	// BridgeBuffer 为 0 时使用默认值。
	BridgeBuffer int
}

// Handler 处理单条 KCP 会话。
type Handler struct {
	cfg    HandlerConfig
	dialer *net.Dialer
	log    *slog.Logger
}

// NewHandler 构造 Handler。
func NewHandler(cfg HandlerConfig) *Handler {
	logger := cfg.SessionOptions.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		cfg: cfg,
		dialer: &net.Dialer{
			Timeout:   cfg.DialTimeout,
			KeepAlive: cfg.Keepalive,
		},
		log: logger,
	}
}

// Handle 处理一条已建立的 KCP 连接：认证 → 处理其上的所有流。
func (h *Handler) Handle(ctx context.Context, conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("会话处理 panic",
				log.Event, "panic",
				log.Remote, conn.RemoteAddr().String(),
				"panic", fmt.Sprint(r),
			)
			_ = conn.Close()
		}
	}()

	sess, err := mux.AcceptSession(ctx, conn, h.cfg.SessionOptions)
	if err != nil {
		h.log.Warn("会话认证失败",
			log.Event, "auth_failed", log.Remote, conn.RemoteAddr().String(), "err", err)
		return
	}
	defer func() { _ = sess.Close() }()

	// CONNECT_REQ 由会话读循环分发，必须立刻返回，因此拨号放到独立 goroutine。
	sess.Handle(protocol.TypeConnectReq, func(s *mux.Session, f protocol.Frame) {
		go h.handleConnect(ctx, s, f)
	})

	select {
	case <-sess.Done():
	case <-ctx.Done():
		sendCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = sess.SendControl(sendCtx, protocol.TypeGoAway, nil)
		cancel()
		time.Sleep(goAwayFlushDelay)
	}

	st := sess.Stats()
	h.log.Info("会话结束",
		log.Event, "session_end",
		log.SessionID, st.Conv,
		log.Remote, st.Remote,
		"frames_in", st.FramesIn,
		"frames_out", st.FramesOut,
	)
}

// handleConnect 处理一条 CONNECT_REQ：登记流 → 拨号目标 → 回 CONNECT_RESP → 双向转发。
func (h *Handler) handleConnect(ctx context.Context, sess *mux.Session, f protocol.Frame) {
	req, err := protocol.ParseConnectRequest(f.Payload)
	if err != nil {
		h.log.Warn("CONNECT_REQ 解析失败",
			log.Event, "connect_parse_failed",
			log.SessionID, sess.Conv(), log.StreamID, f.StreamID, "err", err)
		h.reply(sess, f.StreamID, protocol.RepAddrTypeNotSupported)
		return
	}
	target := req.Address

	stream, err := sess.RegisterStream(f.StreamID)
	if err != nil {
		h.log.Warn("登记流失败",
			log.Event, "register_stream_failed",
			log.SessionID, sess.Conv(), log.StreamID, f.StreamID, "err", err)
		h.reply(sess, f.StreamID, protocol.RepGeneralFailure)
		return
	}
	defer func() { _ = stream.Close() }()

	if target.Port == 0 {
		h.log.Warn("目标端口为 0，拒绝拨号",
			log.Event, "connect_bad_port", log.Target, target.String())
		h.reply(sess, f.StreamID, protocol.RepGeneralFailure)
		_ = stream.Reset()
		return
	}

	dialCtx, cancel := context.WithTimeout(ctx, h.cfg.DialTimeout)
	defer cancel()
	conn, err := h.dialer.DialContext(dialCtx, "tcp", target.Address())
	if err != nil {
		rep := protocol.ReplyFromDialError(err)
		h.log.Warn("拨号目标失败",
			log.Event, "dial_failed",
			log.SessionID, sess.Conv(), log.StreamID, f.StreamID,
			log.Target, target.String(), "reply", protocol.RepString(rep), "err", err)
		h.reply(sess, f.StreamID, rep)
		_ = stream.Reset()
		return
	}
	defer func() { _ = conn.Close() }()

	bind := localBind(conn)
	h.replyBind(sess, f.StreamID, protocol.RepSuccess, bind)
	h.log.Info("目标已连接",
		log.Event, "proxy_up",
		log.SessionID, sess.Conv(), log.StreamID, f.StreamID,
		log.Target, target.String(), "bind", bind.String(),
	)

	if err := mux.Bridge(ctx, conn, stream, h.cfg.BridgeBuffer); err != nil && !isBenign(err) {
		h.log.Debug("转发结束", log.Event, "proxy_bridge_end", "err", err)
	}
	h.log.Info("目标连接关闭",
		log.Event, "proxy_down",
		log.SessionID, sess.Conv(), log.StreamID, f.StreamID, log.Target, target.String())
}

// reply 发送一个失败码的 CONNECT_RESP。
func (h *Handler) reply(sess *mux.Session, streamID uint32, rep byte) {
	h.replyBind(sess, streamID, rep, protocol.UnspecificBind())
}

// replyBind 发送带 BND 地址的 CONNECT_RESP。
func (h *Handler) replyBind(sess *mux.Session, streamID uint32, rep byte, bind protocol.Address) {
	payload, err := protocol.ConnectResponse{Reply: rep, Bind: bind}.Marshal()
	if err != nil {
		h.log.Warn("CONNECT_RESP 编码失败", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.SendFrame(ctx, protocol.TypeConnectResp, streamID, payload); err != nil {
		h.log.Debug("发送 CONNECT_RESP 失败", "err", err)
	}
}

// localBind 把目标连接的本地地址转成 BND 地址。
func localBind(conn net.Conn) protocol.Address {
	host, portStr, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return protocol.UnspecificBind()
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return protocol.UnspecificBind()
	}
	addr, err := protocol.AddressFromHostPort(host, uint16(port))
	if err != nil {
		return protocol.UnspecificBind()
	}
	return addr
}

// isBenign 判断转发结束是否属于正常的连接收尾。
func isBenign(err error) bool {
	return err == nil ||
		err == net.ErrClosed ||
		err == context.Canceled ||
		err == mux.ErrSessionClosed ||
		err == mux.ErrStreamReset ||
		err == mux.ErrStreamWriteClosed
}
