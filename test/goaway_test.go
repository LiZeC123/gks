// 本文件验证 L1「优雅告别」：客户端池主动回收空闲会话时，服务端应当立刻回收该会话及其上的流，
// 而不是等沉默判死（默认 5×心跳间隔 = 100s）。
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"github.com/LiZeC123/gks/internal/client"
	"github.com/LiZeC123/gks/internal/metrics"
	"github.com/LiZeC123/gks/internal/mux"
	"github.com/LiZeC123/gks/internal/protocol"
	"github.com/LiZeC123/gks/internal/server"
	"github.com/LiZeC123/gks/internal/transport"
)

// TestIdleReclaimReleasesServerSession 走真实的池回收路径：
//
//	并发请求把池撑到 max_sessions → 空闲后池按 idle_timeout 回收到 size
//
// 服务端侧用 metrics.Default 观察（客户端会话计在独立注册表里），因此 SessionsActive 就是
// 服务端持有的会话数。修复前客户端是静默 Close，服务端会一直持有这些会话；修复后应立刻回落。
func TestIdleReclaimReleasesServerSession(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	// 1) 目标 HTTP 服务
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer target.Close()

	// 2) gks 服务端（不设 Metrics → 用 metrics.Default，即服务端视角）
	srvLn, err := transport.Listen("127.0.0.1:0", testTransportOptions())
	if err != nil {
		t.Fatalf("服务端监听失败: %v", err)
	}
	defer srvLn.Close()
	replay, err := protocol.NewReplayCache(4096, 2*time.Minute)
	if err != nil {
		t.Fatalf("重放缓存: %v", err)
	}
	srvHandler := server.NewHandler(server.HandlerConfig{
		SessionOptions: mux.Options{
			PSK:                testPSK,
			AEAD:               protocol.AEADChaCha20Poly1305,
			AuthTimeout:        3 * time.Second,
			TimestampWindow:    time.Minute,
			ReplayCache:        replay,
			StreamStallTimeout: 5 * time.Second,
			Logger:             quiet,
		},
		DialTimeout: 5 * time.Second,
		Keepalive:   30 * time.Second,
	})

	// 3) 客户端 + 池（size=1、max=4、idle_timeout 取小值以便观察回收）
	cliReg := &metrics.Registry{}
	cliOpts := mux.Options{
		PSK:                testPSK,
		AEAD:               protocol.AEADChaCha20Poly1305,
		AuthTimeout:        3 * time.Second,
		HeartbeatInterval:  time.Second,
		HeartbeatMiss:      3,
		StreamStallTimeout: 5 * time.Second,
		Logger:             quiet,
		Metrics:            cliReg,
	}
	dialer := transport.NewDialer(testTransportOptions())
	pool := client.NewSessionPool(client.PoolConfig{
		ServerAddr:     srvLn.Addr().String(),
		Size:           1,
		MaxSessions:    4,
		IdleTimeout:    300 * time.Millisecond,
		StartupJitter:  10 * time.Millisecond,
		ConnectTimeout: 3 * time.Second,
		BackoffMin:     200 * time.Millisecond,
		BackoffMax:     time.Second,
	}, dialer, cliOpts)
	cliHandler := client.NewHandler(client.HandlerConfig{
		ServerAddr:       srvLn.Addr().String(),
		SessionOptions:   cliOpts,
		Sessions:         pool,
		HandshakeTimeout: 5 * time.Second,
		ConnectTimeout:   5 * time.Second,
	}, dialer)

	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("SOCKS5 监听失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tr := &tracker{}
	tr.go_(func() { acceptLoop(srvLn, func(c net.Conn) { srvHandler.Handle(ctx, c) }, tr) })
	tr.go_(func() { acceptLoop(socksLn, func(c net.Conn) { cliHandler.Handle(ctx, c) }, tr) })
	pool.Start(ctx)
	defer func() {
		cancel()
		_ = socksLn.Close()
		_ = srvLn.Close()
		pool.Close()
		tr.wait()
	}()

	base := metrics.Default.SessionsActive.Load()

	// 4) 并发 4 条请求，把池撑到 max_sessions
	d, err := proxy.SOCKS5("tcp", socksLn.Addr().String(), nil, proxy.Direct)
	if err != nil {
		t.Fatalf("构造 SOCKS5 dialer: %v", err)
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		t.Fatal("SOCKS5 dialer 未实现 ContextDialer")
	}
	hc := &http.Client{
		Transport: &http.Transport{DialContext: cd.DialContext, DisableKeepAlives: true},
		Timeout:   15 * time.Second,
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := hc.Get(target.URL)
			if err != nil {
				t.Errorf("GET: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()

	waitFor(t, 5*time.Second, func() bool { return pool.Stats().Sessions == 4 },
		"并发后池应撑到 max_sessions=4")
	peakServer := metrics.Default.SessionsActive.Load() - base
	if peakServer != 4 {
		t.Fatalf("并发峰值：服务端会话净增 = %d，期望 4", peakServer)
	}

	// 5) 等池把空闲会话回收（idle_timeout=300ms），服务端应随之回落到保底 1 条
	waitFor(t, 10*time.Second, func() bool { return pool.Stats().Sessions <= 1 },
		"池未把空闲会话回收到 size=1")
	waitFor(t, 5*time.Second, func() bool { return metrics.Default.SessionsActive.Load()-base <= 1 },
		"服务端没有随客户端回收而释放会话（GOAWAY 未生效，只能等沉默判死）")

	got := metrics.Default.SessionsActive.Load() - base
	t.Logf("峰值服务端会话=%d；回收后客户端池=%d、服务端会话=%d",
		peakServer, pool.Stats().Sessions, got)
	if got != 1 {
		t.Fatalf("回收后服务端会话净增 = %d，期望 1", got)
	}
}

// waitFor 在超时内轮询条件，失败时给出说明。
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s（等待上限 %s）", msg, d)
}
