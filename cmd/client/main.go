// Command client 是 gks 的客户端：在本地监听 SOCKS5，把 CONNECT 请求
// 经 KCP（认证 + AEAD）转发给服务端。
//
// 阶段三形态（dev.md §8）：每条本地 SOCKS5 连接使用一条独立 KCP Session，
// 会话上开一条流承载该连接（连接池见阶段 5，多路复用见阶段 4）。
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
	flag.StringVar(&cfgPath, "c", "gks.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.ValidateClient(); err != nil {
		return fmt.Errorf("配置校验失败:\n%w", err)
	}

	logger, err := log.New(os.Stderr, cfg.Client.Log.Level)
	if err != nil {
		return err
	}
	psk, err := cfg.Common.PSKBytes()
	if err != nil {
		return err
	}

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

	handler := client.NewHandler(client.HandlerConfig{
		ServerAddr: cfg.Client.KCP.Server,
		SessionOptions: mux.Options{
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
		},
		HandshakeTimeout: cfg.Client.Socks5.HandshakeTimeout.D(),
		ConnectTimeout:   cfg.Client.Socks5.ConnectTimeout.D(),
	}, dialer)

	ln, err := net.Listen("tcp", cfg.Client.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", cfg.Client.Listen, err)
	}
	defer func() { _ = ln.Close() }()

	logger.Info("gks 客户端已启动",
		log.Event, "client_start",
		"listen", ln.Addr().String(),
		"server", cfg.Client.KCP.Server,
		"crypt", cfg.Common.CryptName(),
		"aead", cfg.Common.AEADName(),
		"heartbeat_interval", cfg.Client.KCP.HeartbeatInterval.D().String(),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		logger.Info("收到退出信号，停止接受新连接", log.Event, "shutdown")
		_ = ln.Close()
	}()

	// 传输统计：每 metrics_interval 打一行（0 表示关闭）。
	sampler := metrics.NewSampler(metrics.Default, cfg.Common.MetricsInterval.D(), logger, transport.SnmpStats)
	go sampler.Run(ctx)

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
	// 退出前再打一条汇总（含累计会话/流数与错误计数）。
	sampler.Log(sampler.Sample())
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
