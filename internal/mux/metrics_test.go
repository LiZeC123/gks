package mux

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/metrics"
)

// TestSessionMetricsAccounting 验证 mux 层把会话/流/载荷字节准确累加到注册表。
func TestSessionMetricsAccounting(t *testing.T) {
	reg := &metrics.Registry{}
	cli, srv := newSessionPair(t, func(o *Options) { o.Metrics = reg })

	if got := reg.SessionsActive.Load(); got != 2 {
		t.Fatalf("活跃会话 = %d，期望 2", got)
	}
	if got := reg.SessionsTotal.Load(); got != 2 {
		t.Fatalf("累计会话 = %d，期望 2", got)
	}

	ctx := context.Background()
	st, err := cli.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	srvSt, err := srv.RegisterStream(st.StreamID())
	if err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}
	if got := reg.StreamsActive.Load(); got != 2 {
		t.Fatalf("活跃流 = %d，期望 2（两端各一条）", got)
	}

	payload := bytes.Repeat([]byte{0xC3}, 8192)
	if _, err := st.Write(payload); err != nil {
		t.Fatalf("写: %v", err)
	}
	if got := readFull(t, srvSt, len(payload), 5*time.Second); !bytes.Equal(got, payload) {
		t.Fatalf("收到 %d 字节，期望 %d", len(got), len(payload))
	}

	// 载荷字节实时累加（与线速率同窗口可比）。
	if got := reg.PayloadSent.Load(); got != uint64(len(payload)) {
		t.Fatalf("PayloadSent = %d，期望 %d（实时累加）", got, len(payload))
	}
	if got := reg.PayloadReceived.Load(); got != uint64(len(payload)) {
		t.Fatalf("PayloadReceived = %d，期望 %d（实时累加）", got, len(payload))
	}
	if err := st.Close(); err != nil {
		t.Fatalf("客户端 Close: %v", err)
	}
	if err := srvSt.Close(); err != nil {
		t.Fatalf("服务端 Close: %v", err)
	}
	if err := cli.Close(); err != nil {
		t.Fatalf("关闭客户端会话: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("关闭服务端会话: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if reg.SessionsActive.Load() == 0 && reg.StreamsActive.Load() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := reg.SessionsActive.Load(); got != 0 {
		t.Fatalf("活跃会话未归零: %d", got)
	}
	if got := reg.StreamsActive.Load(); got != 0 {
		t.Fatalf("活跃流未归零: %d", got)
	}
	if got := reg.PayloadSent.Load(); got != uint64(len(payload)) {
		t.Fatalf("PayloadSent = %d，期望 %d", got, len(payload))
	}
	if got := reg.PayloadReceived.Load(); got != uint64(len(payload)) {
		t.Fatalf("PayloadReceived = %d，期望 %d", got, len(payload))
	}
	if got := reg.StreamsTotal.Load(); got != 2 {
		t.Fatalf("累计流 = %d，期望 2", got)
	}
}

// TestSessionMetricsAuthFailure 验证认证失败计入 AuthFailures（通过 Options 注入注册表）。
func TestSessionMetricsAuthFailure(t *testing.T) {
	reg := &metrics.Registry{}
	cliConn, srvConn := net.Pipe()

	srvOpts := serverOptions(newReplayCache(t), testPSK, 0)
	srvOpts.Metrics = reg
	cliOpts := clientOptions([]byte("ffffffffffffffffffffffffffffffff"), 0)
	cliOpts.Metrics = reg

	errCh := make(chan error, 1)
	go func() {
		_, err := AcceptSession(context.Background(), srvConn, srvOpts)
		errCh <- err
	}()

	if _, err := DialSession(context.Background(), cliConn, cliOpts); err == nil {
		t.Fatal("错误 PSK 不应握手成功")
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("服务端不应握手成功")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待服务端结果超时")
	}
	if got := reg.AuthFailures.Load(); got < 2 {
		t.Fatalf("AuthFailures = %d，期望两端各计一次", got)
	}
}
