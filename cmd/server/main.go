// Command server 是 gks 的服务端。
//
// 当前为阶段二形态（dev.md §8）：接受 KCP 会话、完成认证握手（AUTH_REQ/AUTH_RESP）、
// 回显 Session 级 echo 帧并响应心跳。阶段三起将由「拨号目标并转发」取代 echo。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/LiZeC123/gks/internal/config"
	"github.com/LiZeC123/gks/internal/log"
	"github.com/LiZeC123/gks/internal/mux"
	"github.com/LiZeC123/gks/internal/protocol"
	"github.com/LiZeC123/gks/internal/transport"
)

// goAwayFlushDelay 是发送 GOAWAY 后留给写循环的冲刷时间（阶段二的简化实现，
// 阶段六会替换为真正的「等待活跃流结束」）。
const goAwayFlushDelay = 200 * time.Millisecond

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gks-server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var cfgPath string
	flag.StringVar(&cfgPath, "c", "gks.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Server.Validate(); err != nil {
		return fmt.Errorf("配置校验失败:\n%w", err)
	}

	logger, err := log.New(os.Stderr, cfg.Server.Log.Level)
	if err != nil {
		return err
	}
	psk, err := cfg.Server.PSKBytes()
	if err != nil {
		return err
	}
	replay, err := protocol.NewReplayCache(
		cfg.Server.Auth.ReplayCacheSize,
		2*cfg.Server.Auth.TimestampWindow.D(),
	)
	if err != nil {
		return err
	}

	tln, err := transport.Listen(cfg.Server.Listen, transport.Options{
		Interval:     cfg.Server.KCP.Interval.D(),
		MTU:          cfg.Server.KCP.MTU,
		SndWnd:       cfg.Server.KCP.SndWnd,
		RcvWnd:       cfg.Server.KCP.RcvWnd,
		DataShards:   cfg.Server.KCP.DataShards,
		ParityShards: cfg.Server.KCP.ParityShards,
		Crypt:        cfg.Server.CryptName(),
		PSK:          psk,
	})
	if err != nil {
		return err
	}
	defer func() { _ = tln.Close() }()

	logger.Info("gks 服务端已启动",
		log.Event, "server_start",
		"listen", tln.Addr().String(),
		"crypt", cfg.Server.CryptName(),
		"aead", cfg.Server.AEADName(),
	)

	// 服务端不发心跳（配置中没有该字段），只负责响应 PING；客户端负责保活。
	opts := mux.Options{
		PSK:             psk,
		AEAD:            cfg.Server.AEADName(),
		AuthTimeout:     cfg.Server.Auth.AuthTimeout.D(),
		TimestampWindow: cfg.Server.Auth.TimestampWindow.D(),
		ReplayCache:     replay,
		Logger:          logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 未认证会话的并发上限：属于稳定性保护（dev.md §3.4），不是用户层防御。
	pending := make(chan struct{}, cfg.Server.Auth.MaxPendingSessions)

	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		logger.Info("收到退出信号，停止接受新会话", log.Event, "shutdown")
		_ = tln.Close()
	}()

	for {
		conn, err := tln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			logger.Warn("接受会话失败", log.Event, "accept_error", "err", err)
			continue
		}
		select {
		case pending <- struct{}{}:
		default:
			logger.Warn("未认证会话数已达上限，丢弃新会话",
				log.Event, "pending_limit",
				log.Remote, conn.RemoteAddr().String(),
				"limit", cfg.Server.Auth.MaxPendingSessions,
			)
			_ = conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-pending }()
			handleSession(ctx, conn, opts, logger)
		}()
	}

	grace := cfg.Server.ShutdownGrace.D()
	if waitTimeout(&wg, grace) {
		logger.Info("所有会话已结束", log.Event, "server_stop")
	} else {
		logger.Warn("等待活跃会话超时，强制退出", log.Event, "server_stop", "grace", grace.String())
	}
	return nil
}

// handleSession 完成一条会话的认证与（阶段二的）echo 处理。
func handleSession(ctx context.Context, conn net.Conn, opts mux.Options, logger *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			// panic 隔离：单个会话的问题不得影响进程（dev.md §11.6）。
			logger.Error("会话处理 panic",
				log.Event, "panic",
				log.Remote, conn.RemoteAddr().String(),
				"panic", fmt.Sprint(r),
			)
			_ = conn.Close()
		}
	}()

	sess, err := mux.AcceptSession(ctx, conn, opts)
	if err != nil {
		logger.Warn("会话认证失败",
			log.Event, "auth_failed",
			log.Remote, conn.RemoteAddr().String(),
			"err", err,
		)
		return
	}
	defer func() { _ = sess.Close() }()

	// 阶段二：Session 级 echo（TypeTestEcho 仅用于联调，阶段三移除）。
	sess.Handle(protocol.TypeTestEcho, func(s *mux.Session, f protocol.Frame) {
		sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.SendControl(sendCtx, protocol.TypeTestEcho, f.Payload); err != nil {
			logger.Debug("echo 回发失败", log.Event, "echo_error", "err", err)
		}
	})

	select {
	case <-sess.Done():
	case <-ctx.Done():
		sendCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = sess.SendControl(sendCtx, protocol.TypeGoAway, nil)
		cancel()
		time.Sleep(goAwayFlushDelay)
	}
	logger.Info("会话结束", log.Event, "session_end", "stats", statsFields(sess))
}

func statsFields(s *mux.Session) []any {
	st := s.Stats()
	return []any{
		log.SessionID, st.Conv,
		log.Remote, st.Remote,
		"frames_in", st.FramesIn,
		"frames_out", st.FramesOut,
		"pings_sent", st.PingsSent,
		"pongs_recv", st.PongsRecv,
	}
}

// waitTimeout 等待 wg 完成，最多等 d。
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}
