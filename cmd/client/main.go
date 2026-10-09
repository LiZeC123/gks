// Command client 是 gks 的客户端：在本地监听 SOCKS5，把 CONNECT 请求
// 经 KCP（认证 + AEAD）转发给服务端。
//
// 阶段三形态（dev.md §8）：每条本地 SOCKS5 连接使用一条独立 KCP Session，
// 会话上开一条流承载该连接（连接池见阶段 5，多路复用见阶段 4）。
//
// 观测：日志只写配置文件指定的文件（为空则丢弃），控制台留给周期刷新的统计表格；
// 统计另经 HTTP 端点（默认 127.0.0.1:12081）以 JSON 暴露，供外部程序周期拉取。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/LiZeC123/gks/internal/client"
	"github.com/LiZeC123/gks/internal/config"
	"github.com/LiZeC123/gks/internal/log"
	"github.com/LiZeC123/gks/internal/metrics"
	"github.com/LiZeC123/gks/internal/monitor"
	"github.com/LiZeC123/gks/internal/mux"
	"github.com/LiZeC123/gks/internal/transport"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gks-client: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var cfgPath string
	var noConsole bool
	flag.StringVar(&cfgPath, "c", "gks.yaml", "配置文件路径")
	flag.BoolVar(&noConsole, "no-console", false, "关闭控制台统计表格（配合空 log.file 即完全静默）")
	flag.Parse()

	startedAt := time.Now()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.ValidateClient(); err != nil {
		return fmt.Errorf("配置校验失败:\n%w", err)
	}

	// 日志只写文件：file 为空时全部丢弃，控制台不再出现日志。
	logger, logCloser, err := log.NewFromConfig(cfg.Client.Log.Level, cfg.Client.Log.File)
	if err != nil {
		return err
	}
	defer func() { _ = logCloser.Close() }()

	psk, err := cfg.Common.PSKBytes()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dialer := transport.NewDialer(transport.Options{
		Interval:     cfg.Common.KCP.Interval.D(),
		MTU:          cfg.Common.KCP.MTU,
		SndWnd:       cfg.Common.KCP.SndWnd,
		RcvWnd:       cfg.Common.KCP.RcvWnd,
		DataShards:   cfg.Common.KCP.DataShards,
		ParityShards: cfg.Common.KCP.ParityShards,
		Crypt:        cfg.Common.CryptName(),
		PSK:          psk,
	})

	sessOpts := mux.Options{
		PSK:               psk,
		AEAD:              cfg.Common.AEADName(),
		AuthTimeout:       cfg.Client.KCP.AuthTimeout.D(),
		HeartbeatInterval: cfg.Client.KCP.HeartbeatInterval.D(),
		HeartbeatMiss:     cfg.Client.KCP.HeartbeatMiss,
		StreamIdleTimeout: cfg.Common.Stream.IdleTimeout.D(),
		MaxStreams:        cfg.Common.Limits.MaxStreamsPerSession,
		MaxDataPayload:    cfg.Common.Limits.MaxDataPayload,
		MaxFrameBody:      cfg.Common.Limits.MaxFrameBody,
		Logger:            logger,
	}

	// 会话池：保底 pool.size 条已认证会话，供本地连接复用（同一时刻每条会话 1 条流）。
	pool := client.NewSessionPool(client.PoolConfig{
		ServerAddr:     cfg.Client.KCP.Server,
		Size:           cfg.Client.Pool.Size,
		MaxSessions:    cfg.Client.Pool.MaxSessions,
		IdleTimeout:    cfg.Client.Pool.IdleTimeout.D(),
		StartupJitter:  cfg.Client.Pool.StartupJitter.D(),
		ConnectTimeout: cfg.Client.Pool.ConnectTimeout.D(),
		BackoffMin:     cfg.Client.Pool.BackoffMin.D(),
		BackoffMax:     cfg.Client.Pool.BackoffMax.D(),
	}, dialer, sessOpts)
	defer pool.Close()

	handler := client.NewHandler(client.HandlerConfig{
		ServerAddr:       cfg.Client.KCP.Server,
		SessionOptions:   sessOpts,
		Sessions:         pool,
		HandshakeTimeout: cfg.Client.Socks5.HandshakeTimeout.D(),
		ConnectTimeout:   cfg.Client.Socks5.ConnectTimeout.D(),
	}, dialer)

	ln, err := net.Listen("tcp", cfg.Client.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", cfg.Client.Listen, err)
	}
	defer func() { _ = ln.Close() }()

	// 观测：1s 粒度的速率历史 + 控制台表格 + 统计 HTTP 端点。
	// 传输层来自 kcp-go 的全局 Snmp，池状态来自会话池，二者都不依赖具体实现细节。
	collector := metrics.NewCollector(metrics.Options{
		Interval: cfg.Common.MetricsInterval.D(),
		Sources: metrics.Sources{
			Transport: transport.SnmpStats,
			Pool: func() metrics.PoolStats {
				st := pool.Stats()
				return metrics.PoolStats{
					Sessions: st.Sessions,
					InUse:    st.InUse,
					Idle:     st.Idle,
					Creating: st.Creating,
					Waiters:  st.Waiters,
					Rebuilds: st.Rebuilds,
				}
			},
		},
	})
	metricsAddr, _ := cfg.Client.Metrics.Addr()
	mon, err := monitor.Start(ctx, monitor.Options{
		Role:       "client",
		Collector:  collector,
		HTTPAddr:   metricsAddr,
		Console:    !noConsole,
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
	logger.Info("gks 客户端已启动",
		log.Event, "client_start",
		"listen", ln.Addr().String(),
		"server", cfg.Client.KCP.Server,
		"crypt", cfg.Common.CryptName(),
		"aead", cfg.Common.AEADName(),
		"heartbeat_interval", cfg.Client.KCP.HeartbeatInterval.D().String(),
		"metrics_listen", metricsListen,
		"metrics_interval", cfg.Common.MetricsInterval.D().String(),
		"log_file", cfg.Client.Log.File,
		"console", !noConsole,
	)

	go func() {
		<-ctx.Done()
		logger.Info("收到退出信号，停止接受新连接", log.Event, "shutdown")
		_ = ln.Close()
	}()

	// 预热并维护会话池（保底重建 + 空闲回收）。
	pool.Start(ctx)

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			logger.Warn("接受本地连接失败", log.Event, "accept_error", "err", err)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler.Handle(ctx, conn)
		}()
	}

	grace := cfg.Client.ShutdownGrace.D()
	if waitTimeout(&wg, grace) {
		logger.Info("所有本地连接已结束", log.Event, "client_stop")
	} else {
		logger.Warn("等待活跃连接超时，强制退出", log.Event, "client_stop", "grace", grace.String())
	}
	// 退出前记录一条汇总（含累计会话/流数与错误计数）。
	ps := pool.Stats()
	logger.Info("会话池状态",
		log.Event, "pool_stats",
		"sessions", ps.Sessions,
		"in_use", ps.InUse,
		"idle", ps.Idle,
		"rebuilds", ps.Rebuilds,
	)
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
