// Command server 是 gks 的服务端。
//
// 阶段三形态（dev.md §8）：接受 KCP 会话、完成认证握手，按 CONNECT_REQ 拨号目标
// 并双向转发（DATA/FIN/RST）。
//
// 观测：默认静默——日志只写配置文件指定的文件（为空即丢弃），控制台不输出任何内容；
// 加 -console 才会在控制台周期刷新统计表格。统计另经 HTTP 端点（默认 127.0.0.1:12081）
// 以 JSON 暴露，供外部程序周期拉取。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/LiZeC123/gks/internal/config"
	"github.com/LiZeC123/gks/internal/log"
	"github.com/LiZeC123/gks/internal/metrics"
	"github.com/LiZeC123/gks/internal/monitor"
	"github.com/LiZeC123/gks/internal/mux"
	"github.com/LiZeC123/gks/internal/protocol"
	"github.com/LiZeC123/gks/internal/server"
	"github.com/LiZeC123/gks/internal/transport"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gks-server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var cfgPath string
	var console bool
	flag.StringVar(&cfgPath, "c", "gks.yaml", "配置文件路径")
	flag.BoolVar(&console, "console", false, "在控制台周期刷新统计表格（默认关闭：不产生任何控制台输出）")
	flag.Parse()

	startedAt := time.Now()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.ValidateServer(); err != nil {
		return fmt.Errorf("配置校验失败:\n%w", err)
	}

	// 日志只写文件：file 为空时全部丢弃，控制台不再出现日志。
	logger, logCloser, err := log.NewFromConfig(cfg.Server.Log.Level, cfg.Server.Log.File)
	if err != nil {
		return err
	}
	defer func() { _ = logCloser.Close() }()

	// 传输层加密若被显式关掉、或换成了不提供完整性的 CFB 系列，启动时提示一次。
	if warn := config.InsecureCryptWarning(cfg.Common.CryptName()); warn != "" {
		logger.Warn(warn, log.Event, "insecure_crypt", "crypt", cfg.Common.CryptName())
	}

	psk, err := cfg.Common.PSKBytes()
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
		Interval:     cfg.Common.KCP.Interval.D(),
		MTU:          cfg.Common.KCP.MTU,
		SndWnd:       cfg.Common.KCP.SndWnd,
		RcvWnd:       cfg.Common.KCP.RcvWnd,
		DataShards:   cfg.Common.KCP.DataShards,
		ParityShards: cfg.Common.KCP.ParityShards,
		Crypt:        cfg.Common.CryptName(),
		PSK:          psk,
	})
	if err != nil {
		return err
	}
	defer func() { _ = tln.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 观测：1s 粒度的速率历史 + 控制台表格 + 统计 HTTP 端点（服务端无会话池指标）。
	collector := metrics.NewCollector(metrics.Options{
		Interval: cfg.Common.MetricsInterval.D(),
		Sources:  metrics.Sources{Transport: transport.SnmpStats},
	})
	metricsAddr, _ := cfg.Server.Metrics.Addr()
	mon, err := monitor.Start(ctx, monitor.Options{
		Role:       "server",
		Collector:  collector,
		HTTPAddr:   metricsAddr,
		Console:    console,
		ConsoleOut: os.Stdout,
		StartedAt:  startedAt,
		Logger:     logger,
	})
	if err != nil {
		return err
	}
	defer mon.Close()

	metricsListen := "disabled"
	if addr := mon.Addr(); addr != "" {
		metricsListen = addr
	}
	logger.Info("gks 服务端已启动",
		log.Event, "server_start",
		"listen", tln.Addr().String(),
		"crypt", cfg.Common.CryptName(),
		"aead", cfg.Common.AEADName(),
		"dial_timeout", cfg.Server.Dial.Timeout.D().String(),
		"metrics_listen", metricsListen,
		"metrics_interval", cfg.Common.MetricsInterval.D().String(),
		"log_file", cfg.Server.Log.File,
		"console", console,
	)

	// 服务端不发心跳，只响应 PING；保活由客户端负责，服务端侧只做被动判活：
	// 连续 5×common.heartbeat.interval（默认 100s）没有任何入站帧就回收该会话。
	handler := server.NewHandler(server.HandlerConfig{
		SessionOptions: mux.Options{
			PSK:                psk,
			AEAD:               cfg.Common.AEADName(),
			AuthTimeout:        cfg.Server.Auth.AuthTimeout.D(),
			TimestampWindow:    cfg.Server.Auth.TimestampWindow.D(),
			ReplayCache:        replay,
			StreamStallTimeout: cfg.Common.Stream.RecvStallTimeout.D(),
			PeerSilenceTimeout: cfg.Common.Heartbeat.SilenceTimeout(),
			MaxStreams:         cfg.Common.Limits.MaxStreamsPerSession,
			MaxDataPayload:     cfg.Common.Limits.MaxDataPayload,
			MaxFrameBody:       cfg.Common.Limits.MaxFrameBody,
			Logger:             logger,
		},
		DialTimeout: cfg.Server.Dial.Timeout.D(),
		Keepalive:   cfg.Server.Dial.Keepalive.D(),
	})

	// 未认证会话的并发上限：属于稳定性保护（dev.md §3.4），不是用户层防御。
	pending := make(chan struct{}, cfg.Server.Auth.MaxPendingSessions)

	var wg sync.WaitGroup

	// acceptPollInterval 是 accept 循环的轮询间隔。
	//
	// 为什么要轮询而不是「退出时关监听」：kcp-go 的 Listener 只有一个共享 UDP socket，Close 会
	// 把它关掉、使所有活跃会话立刻读失败，尚未发出的 GOAWAY 全部丢失（dev.md §15 第 22 条）；
	// 而 AcceptKCP 只在进入时读取监听读期限，已经阻塞的 Accept 不会被随后的 SetReadDeadline
	// 唤醒。轮询因此给「收到退出信号 → 停止接受新会话」一个固定上限（≤ 本间隔），同时保持
	// socket 打开，让在途会话把 GOAWAY 发出去。
	const acceptPollInterval = 200 * time.Millisecond

	for {
		deadline := time.Now().Add(acceptPollInterval)
		_ = tln.SetReadDeadline(deadline)
		conn, err := tln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				logger.Info("收到退出信号，停止接受新会话", log.Event, "shutdown")
				break
			}
			// 只有「早于期限」的错误才是真错误；轮询超时是正常路径。这里比较时间而不是判断
			// 错误类型：kcp-go 各版本对超时的表示不同（v5.4.20 是普通 error，v5.6.72 才实现 net.Error）。
			if time.Now().Before(deadline) {
				logger.Warn("接受会话失败", log.Event, "accept_error", "err", err)
				time.Sleep(50 * time.Millisecond) // 持续性错误不要变成忙等
			}
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
			handler.Handle(ctx, conn)
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
