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

	// 服务端不发心跳（配置中没有该字段），只响应 PING；保活由客户端负责。
	handler := server.NewHandler(server.HandlerConfig{
		SessionOptions: mux.Options{
			PSK:               psk,
			AEAD:              cfg.Common.AEADName(),
			AuthTimeout:       cfg.Server.Auth.AuthTimeout.D(),
			TimestampWindow:   cfg.Server.Auth.TimestampWindow.D(),
			ReplayCache:       replay,
			StreamIdleTimeout: cfg.Common.Stream.IdleTimeout.D(),
			MaxStreams:        cfg.Common.Limits.MaxStreamsPerSession,
			MaxDataPayload:    cfg.Common.Limits.MaxDataPayload,
			MaxFrameBody:      cfg.Common.Limits.MaxFrameBody,
			Logger:            logger,
		},
		DialTimeout: cfg.Server.Dial.Timeout.D(),
		Keepalive:   cfg.Server.Dial.Keepalive.D(),
	})

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
