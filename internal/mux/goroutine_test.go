package mux

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/transport"
)

// TestSessionGoroutineFootprint 测量「每连接一条 KCP Session」的真实资源开销，
// 并验证会话关闭后 goroutine 能回落到基线（dev.md §9.4 / §13.5 的种子测试）。
//
// 这条测试是阶段 4/5 收益讨论的量化依据：它给出的数字就是连接池能省掉的那部分。
func TestSessionGoroutineFootprint(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	tOpts := transport.Options{
		Interval: 10 * time.Millisecond,
		MTU:      1350,
		SndWnd:   64,
		RcvWnd:   64,
		Crypt:    "none",
	}

	ln, err := transport.Listen("127.0.0.1:0", tOpts)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var (
		mu       sync.Mutex
		accepted []*Session
	)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				opts := serverOptions(newReplayCache(t), testPSK, 0)
				opts.Logger = quiet
				s, err := AcceptSession(context.Background(), conn, opts)
				if err != nil {
					return
				}
				mu.Lock()
				accepted = append(accepted, s)
				mu.Unlock()
			}()
		}
	}()

	settle := func() {
		for i := 0; i < 40; i++ {
			runtime.GC()
			time.Sleep(25 * time.Millisecond)
		}
	}
	settle()
	base := runtime.NumGoroutine()

	const n = 20
	clients := make([]*Session, 0, n)
	for i := 0; i < n; i++ {
		conn, err := transport.NewDialer(tOpts).Dial(ln.Addr().String())
		if err != nil {
			t.Fatalf("第 %d 条 Dial: %v", i+1, err)
		}
		opts := clientOptions(testPSK, 0)
		opts.Logger = quiet
		s, err := DialSession(context.Background(), conn, opts)
		if err != nil {
			t.Fatalf("第 %d 条 DialSession: %v", i+1, err)
		}
		clients = append(clients, s)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		got := len(accepted)
		mu.Unlock()
		if got == n || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	after := runtime.NumGoroutine()
	// 每条连接在两端各有一条会话，因此除以 2n。
	perConn := float64(after-base) / float64(n)
	t.Logf("每条连接（两端合计）新增 goroutine ≈ %.1f；%d 条连接共 %d → %d",
		perConn, n, base, after)

	if perConn > 16 {
		t.Fatalf("每条连接的 goroutine 开销 %.1f 超出预期上限 16", perConn)
	}

	// 全部关闭后应回落到基线附近。
	for _, s := range clients {
		_ = s.Close()
	}
	mu.Lock()
	servers := append([]*Session(nil), accepted...)
	mu.Unlock()
	for _, s := range servers {
		_ = s.Close()
	}
	_ = ln.Close()

	recovered := false
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base+8 {
			recovered = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !recovered {
		t.Fatalf("会话关闭后 goroutine 未回落: base=%d now=%d", base, runtime.NumGoroutine())
	}
	t.Logf("全部关闭后 goroutine 回落至 %d（基线 %d）", runtime.NumGoroutine(), base)
}
