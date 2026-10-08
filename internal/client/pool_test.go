package client

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/metrics"
	"github.com/LiZeC123/gks/internal/mux"
	"github.com/LiZeC123/gks/internal/protocol"
	"github.com/LiZeC123/gks/internal/transport"
)

var testPSK = []byte("0123456789abcdef0123456789abcdef")

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// tracker 记账动态 goroutine，保证 Add 不会与 Wait 竞争。
type tracker struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	closed bool
}

func (tr *tracker) go_(fn func()) {
	tr.mu.Lock()
	if tr.closed {
		tr.mu.Unlock()
		return
	}
	tr.wg.Add(1)
	tr.mu.Unlock()
	go func() {
		defer tr.wg.Done()
		fn()
	}()
}

func (tr *tracker) wait() {
	tr.mu.Lock()
	tr.closed = true
	tr.mu.Unlock()
	tr.wg.Wait()
}

// testServer 是一个只做「接受会话并保持」的极简服务端。
type testServer struct {
	addr string
	stop func()
}

func newTestServer(t *testing.T, addr string) *testServer {
	t.Helper()
	ln, err := transport.Listen(addr, transport.Options{
		Interval: 10 * time.Millisecond,
		MTU:      1350,
		SndWnd:   64,
		RcvWnd:   64,
	})
	if err != nil {
		t.Fatalf("Listen(%q): %v", addr, err)
	}
	replay, err := protocol.NewReplayCache(512, time.Minute)
	if err != nil {
		t.Fatalf("NewReplayCache: %v", err)
	}
	opts := mux.Options{
		PSK:             testPSK,
		AEAD:            protocol.AEADChaCha20Poly1305,
		AuthTimeout:     500 * time.Millisecond,
		TimestampWindow: time.Minute,
		ReplayCache:     replay,
		Logger:          quietLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	tr := &tracker{}
	tr.go_(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			tr.go_(func() {
				s, err := mux.AcceptSession(ctx, conn, opts)
				if err != nil {
					return
				}
				<-s.Done()
			})
		}
	})
	srv := &testServer{addr: ln.Addr().String()}
	srv.stop = func() {
		cancel()
		_ = ln.Close()
		tr.wait()
	}
	return srv
}

func testPoolOptions(reg *metrics.Registry, heartbeat time.Duration) mux.Options {
	return mux.Options{
		PSK:               testPSK,
		AEAD:              protocol.AEADChaCha20Poly1305,
		AuthTimeout:       500 * time.Millisecond,
		HeartbeatInterval: heartbeat,
		HeartbeatMiss:     3,
		Metrics:           reg,
		Logger:            quietLogger(),
	}
}

func newTestPool(t *testing.T, srv *testServer, cfg PoolConfig, reg *metrics.Registry, heartbeat time.Duration) *SessionPool {
	t.Helper()
	cfg.ServerAddr = srv.addr
	if cfg.BackoffMin == 0 {
		cfg.BackoffMin = 100 * time.Millisecond
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = 500 * time.Millisecond
	}
	dialer := transport.NewDialer(transport.Options{
		Interval: 10 * time.Millisecond,
		MTU:      1350,
		SndWnd:   64,
		RcvWnd:   64,
	})
	pool := NewSessionPool(cfg, dialer, testPoolOptions(reg, heartbeat))
	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)
	t.Cleanup(func() {
		cancel()
		pool.Close()
	})
	return pool
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", what)
}

func TestPoolWarmsUpToSize(t *testing.T) {
	srv := newTestServer(t, "127.0.0.1:0")
	defer srv.stop()
	pool := newTestPool(t, srv, PoolConfig{
		Size:           2,
		MaxSessions:    4,
		StartupJitter:  20 * time.Millisecond,
		ConnectTimeout: 2 * time.Second,
	}, nil, 0)

	waitFor(t, "保底预热到 2 条", 5*time.Second, func() bool { return pool.Stats().Sessions == 2 })
	st := pool.Stats()
	if st.Idle != 2 || st.InUse != 0 {
		t.Fatalf("预热后状态 = %+v，期望 2 条空闲", st)
	}
}

func TestPoolReusesIdleSession(t *testing.T) {
	reg := &metrics.Registry{}
	srv := newTestServer(t, "127.0.0.1:0")
	defer srv.stop()
	pool := newTestPool(t, srv, PoolConfig{
		Size:           1,
		MaxSessions:    2,
		ConnectTimeout: 2 * time.Second,
	}, reg, 0)

	waitFor(t, "保底会话就绪", 5*time.Second, func() bool { return pool.Stats().Idle == 1 })

	for i := 1; i <= 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		sess, err := pool.Acquire(ctx)
		cancel()
		if err != nil {
			t.Fatalf("第 %d 次 Acquire: %v", i, err)
		}
		if st := pool.Stats(); st.InUse != 1 {
			t.Fatalf("第 %d 次 Acquire 后 InUse = %d", i, st.InUse)
		}
		pool.Release(sess)
	}

	if got := reg.SessionsTotal.Load(); got != 1 {
		t.Fatalf("累计会话 = %d，期望 1（空闲会话应被复用）", got)
	}
	st := pool.Stats()
	if st.Sessions != 1 || st.Rebuilds != 0 {
		t.Fatalf("池状态 = %+v", st)
	}
}

func TestPoolMaxSessionsAndTimeout(t *testing.T) {
	srv := newTestServer(t, "127.0.0.1:0")
	defer srv.stop()
	pool := newTestPool(t, srv, PoolConfig{
		Size:           1,
		MaxSessions:    2,
		ConnectTimeout: 400 * time.Millisecond,
	}, nil, 0)

	waitFor(t, "保底会话就绪", 5*time.Second, func() bool { return pool.Stats().Idle == 1 })

	ctx := context.Background()
	first, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("第 1 次 Acquire: %v", err)
	}
	second, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("第 2 次 Acquire（新建）: %v", err)
	}
	if _, err := pool.Acquire(ctx); !errors.Is(err, ErrPoolTimeout) {
		t.Fatalf("达到上限后 err = %v，期望 ErrPoolTimeout", err)
	}

	// 归还一条后应能立刻取到。
	pool.Release(first)
	third, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("归还后 Acquire: %v", err)
	}
	pool.Release(second)
	pool.Release(third)
}

func TestPoolReclaimsIdleBeyondSize(t *testing.T) {
	srv := newTestServer(t, "127.0.0.1:0")
	defer srv.stop()
	pool := newTestPool(t, srv, PoolConfig{
		Size:           1,
		MaxSessions:    3,
		IdleTimeout:    150 * time.Millisecond,
		ConnectTimeout: 2 * time.Second,
	}, nil, 0)

	waitFor(t, "保底会话就绪", 5*time.Second, func() bool { return pool.Stats().Idle == 1 })

	ctx := context.Background()
	held := make([]*mux.Session, 0, 3)
	for i := 0; i < 3; i++ {
		s, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("第 %d 次 Acquire: %v", i+1, err)
		}
		held = append(held, s)
	}
	if st := pool.Stats(); st.Sessions != 3 || st.InUse != 3 {
		t.Fatalf("持有状态 = %+v，期望 3 条全部在使用中", st)
	}
	for _, s := range held {
		pool.Release(s)
	}
	waitFor(t, "空闲回收回落到 size", 5*time.Second, func() bool { return pool.Stats().Sessions == 1 })
	st := pool.Stats()
	if st.Idle != 1 {
		t.Fatalf("回收后状态 = %+v，期望仅剩 1 条空闲保底会话", st)
	}
}

func TestPoolDiscardsDeadSession(t *testing.T) {
	srv := newTestServer(t, "127.0.0.1:0")
	pool := newTestPool(t, srv, PoolConfig{
		Size:           1,
		MaxSessions:    2,
		ConnectTimeout: 500 * time.Millisecond,
	}, nil, 200*time.Millisecond)

	waitFor(t, "保底会话就绪", 5*time.Second, func() bool { return pool.Stats().Idle == 1 })
	sess, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	srv.stop() // 服务端下线：会话会因心跳缺失被判死

	select {
	case <-sess.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("会话未在服务端下线后失效")
	}
	waitFor(t, "池摘除失效会话", 5*time.Second, func() bool { return pool.Stats().Sessions == 0 })

	// 归还有问题的会话不应把它留在池里。
	pool.Release(sess)
	if st := pool.Stats(); st.Sessions != 0 {
		t.Fatalf("失效会话未清理: %+v", st)
	}
}

func TestPoolRebuildsAfterServerRestart(t *testing.T) {
	srv := newTestServer(t, "127.0.0.1:0")
	addr := srv.addr
	pool := newTestPool(t, srv, PoolConfig{
		Size:           1,
		MaxSessions:    2,
		ConnectTimeout: 2 * time.Second,
	}, nil, 200*time.Millisecond)

	waitFor(t, "保底会话就绪", 5*time.Second, func() bool { return pool.Stats().Idle == 1 })

	srv.stop()
	waitFor(t, "会话全部失效", 10*time.Second, func() bool { return pool.Stats().Sessions == 0 })

	// 同端口重启服务端，池应在退避后自动恢复保底数量。
	restarted := newTestServer(t, addr)
	defer restarted.stop()

	waitFor(t, "保底会话重建", 15*time.Second, func() bool { return pool.Stats().Sessions >= 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("重建后 Acquire: %v", err)
	}
	pool.Release(sess)
}

func TestPoolCloseFailsAcquire(t *testing.T) {
	srv := newTestServer(t, "127.0.0.1:0")
	defer srv.stop()
	pool := newTestPool(t, srv, PoolConfig{
		Size:           1,
		MaxSessions:    2,
		ConnectTimeout: 300 * time.Millisecond,
	}, nil, 0)

	waitFor(t, "保底会话就绪", 5*time.Second, func() bool { return pool.Stats().Idle == 1 })
	pool.Close()
	if st := pool.Stats(); st.Sessions != 0 {
		t.Fatalf("关闭后仍有会话: %+v", st)
	}
	if _, err := pool.Acquire(context.Background()); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("关闭后 Acquire err = %v，期望 ErrPoolClosed", err)
	}
	pool.Close() // 幂等
}

func TestPoolStatsAndSignals(t *testing.T) {
	// 直接检验信号广播：等待者应被 Release 唤醒（而不是靠轮询兜底）。
	srv := newTestServer(t, "127.0.0.1:0")
	defer srv.stop()
	pool := newTestPool(t, srv, PoolConfig{
		Size:           1,
		MaxSessions:    1,
		ConnectTimeout: 3 * time.Second,
	}, nil, 0)

	waitFor(t, "保底会话就绪", 5*time.Second, func() bool { return pool.Stats().Idle == 1 })
	sess, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	got := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s2, err := pool.Acquire(ctx)
		if err == nil {
			pool.Release(s2)
		}
		got <- err
	}()

	time.Sleep(100 * time.Millisecond) // 让等待者先进入等待
	pool.Release(sess)

	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("等待者未被唤醒: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待者未在归还后被唤醒")
	}
}
