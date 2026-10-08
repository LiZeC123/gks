package metrics

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// syncBuffer 是并发安全的日志缓冲（Run 在另一个 goroutine 里写）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestHumanRate(t *testing.T) {
	cases := []struct {
		bps  float64
		want string
	}{
		{0, "0 B/s"},
		{512, "512 B/s"},
		{2048, "2.0 KB/s"},
		{214 * 1024 * 1024, "214.0 MB/s"},
		{2.5 * 1024 * 1024 * 1024, "2.50 GB/s"},
	}
	for _, tc := range cases {
		if got := HumanRate(tc.bps); got != tc.want {
			t.Fatalf("HumanRate(%v) = %q，期望 %q", tc.bps, got, tc.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		b    float64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{2048, "2.0 KB"},
		{104857600, "100.0 MB"},
		{2.5 * 1024 * 1024 * 1024, "2.50 GB"},
	}
	for _, tc := range cases {
		if got := HumanBytes(tc.b); got != tc.want {
			t.Fatalf("HumanBytes(%v) = %q，期望 %q", tc.b, got, tc.want)
		}
	}
}

func TestRegistryTake(t *testing.T) {
	reg := &Registry{}
	reg.SessionsActive.Add(3)
	reg.StreamsActive.Add(7)
	reg.SessionsTotal.Add(10)
	reg.StreamsTotal.Add(20)
	reg.PayloadSent.Add(111)
	reg.PayloadReceived.Add(222)
	reg.AuthFailures.Add(1)
	reg.SessionDialFailures.Add(2)
	reg.Socks5Failures.Add(3)
	reg.DialFailures.Add(4)

	now := time.Unix(1700000000, 0)
	ts := TransportStats{UDPBytesSent: 900, OutSegs: 42}
	pool := PoolStats{Sessions: 3, InUse: 2, Idle: 1, Creating: 1, Waiters: 4, Rebuilds: 5}
	snap := reg.Take(ts, pool, now)

	if !snap.At.Equal(now) || snap.Transport != ts {
		t.Fatalf("快照元数据不对: %+v", snap)
	}
	if snap.Pool != pool {
		t.Fatalf("池快照 = %+v，期望 %+v", snap.Pool, pool)
	}
	r := snap.Registry
	if r.SessionsActive != 3 || r.StreamsActive != 7 || r.SessionsTotal != 10 || r.StreamsTotal != 20 {
		t.Fatalf("计数不对: %+v", r)
	}
	if r.PayloadSent != 111 || r.PayloadReceived != 222 {
		t.Fatalf("载荷字节不对: %+v", r)
	}
	if r.AuthFailures != 1 || r.SessionDialFailures != 2 || r.Socks5Failures != 3 || r.DialFailures != 4 {
		t.Fatalf("错误计数不对: %+v", r)
	}
}

func TestSamplerRates(t *testing.T) {
	reg := &Registry{}
	reg.PayloadSent.Add(1000)
	reg.PayloadReceived.Add(500)

	transport := TransportStats{
		UDPBytesSent:     10_000,
		UDPBytesReceived: 20_000,
		OutSegs:          1_000,
		InSegs:           900,
		RetransSegs:      10,
		LostSegs:         4,
		RepeatSegs:       2,
		KCPInErrors:      1,
		FECRecovered:     100,
		FECErrs:          1,
	}
	var cur TransportStats
	s := NewSampler(reg, time.Second, discardLogger(), Sources{Transport: func() TransportStats { return cur }})
	cur = transport

	t0 := time.Unix(1700000000, 0)
	first := s.sampleAt(t0)
	if first.Interval != 0 || first.PayloadSentBps != 0 || first.WireSentBps != 0 {
		t.Fatalf("首次采样应无速率: %+v", first)
	}
	if first.Current.Registry.PayloadSent != 1000 {
		t.Fatalf("首次快照应含当时计数: %+v", first.Current.Registry)
	}

	// 2 秒后：载荷 +2048/+1024，UDP +10MB/+5MB，段 +200/+100，重传 +20/丢包 +6/重复 +3
	reg.PayloadSent.Add(2048)
	reg.PayloadReceived.Add(1024)
	cur = TransportStats{
		UDPBytesSent:     10_000 + 10*1024*1024,
		UDPBytesReceived: 20_000 + 5*1024*1024,
		OutSegs:          1_000 + 200,
		InSegs:           900 + 100,
		RetransSegs:      10 + 20,
		LostSegs:         4 + 6,
		RepeatSegs:       2 + 3,
		KCPInErrors:      1 + 1,
		FECRecovered:     100 + 40,
		FECErrs:          1 + 2,
	}
	second := s.sampleAt(t0.Add(2 * time.Second))

	if second.Interval != 2*time.Second {
		t.Fatalf("间隔 = %s", second.Interval)
	}
	if second.PayloadSentBps != 1024 || second.PayloadRecvBps != 512 {
		t.Fatalf("载荷速率 = %v / %v", second.PayloadSentBps, second.PayloadRecvBps)
	}
	if want := float64(10 * 1024 * 1024 / 2); second.WireSentBps != want {
		t.Fatalf("线速率 = %v，期望 %v", second.WireSentBps, want)
	}
	if want := float64(5 * 1024 * 1024 / 2); second.WireRecvBps != want {
		t.Fatalf("线接收速率 = %v，期望 %v", second.WireRecvBps, want)
	}
	if second.RetransRate != 0.1 {
		t.Fatalf("重传率 = %v，期望 0.1", second.RetransRate)
	}
	if second.OutSegsDelta != 200 {
		t.Fatalf("发送段增量 = %d，期望 200", second.OutSegsDelta)
	}
	if second.LostSegsDelta != 6 || second.RepeatSegsDelta != 3 || second.KCPInErrorsDelta != 1 {
		t.Fatalf("段增量不对: %+v", second)
	}
	if second.FECRecoveredDelta != 40 || second.FECErrsDelta != 2 {
		t.Fatalf("FEC 增量不对: recovered=%d errs=%d", second.FECRecoveredDelta, second.FECErrsDelta)
	}
}

func TestSamplerZeroIntervalSample(t *testing.T) {
	reg := &Registry{}
	reg.PayloadSent.Add(100)
	cur := TransportStats{UDPBytesSent: 1000, OutSegs: 10}
	s := NewSampler(reg, time.Second, discardLogger(), Sources{Transport: func() TransportStats { return cur }})
	now := time.Unix(1700000000, 0)
	_ = s.sampleAt(now)
	reg.PayloadSent.Add(100)
	cur = TransportStats{UDPBytesSent: 2000, OutSegs: 20}
	smp := s.sampleAt(now) // 同一时刻
	if smp.Interval != 0 || smp.PayloadSentBps != 0 || smp.RetransRate != 0 {
		t.Fatalf("零间隔应无速率: %+v", smp)
	}
}

func TestSamplerHandlesCounterReset(t *testing.T) {
	reg := &Registry{}
	reg.PayloadSent.Add(1000)
	cur := TransportStats{UDPBytesSent: 5000, OutSegs: 100, RetransSegs: 5}
	s := NewSampler(reg, time.Second, discardLogger(), Sources{Transport: func() TransportStats { return cur }})
	now := time.Unix(1700000000, 0)
	_ = s.sampleAt(now)

	// 计数器回退（例如对端重启导致 Snmp 重置）：增量应为 0 而不是负数。
	reg.PayloadSent.Store(10)
	cur = TransportStats{UDPBytesSent: 1, OutSegs: 1, RetransSegs: 0}
	smp := s.sampleAt(now.Add(time.Second))
	if smp.PayloadSentBps != 0 || smp.WireSentBps != 0 || smp.RetransRate != 0 {
		t.Fatalf("计数器回退时应为 0: %+v", smp)
	}
}

func TestSamplerRetransRateWithoutOutSegs(t *testing.T) {
	reg := &Registry{}
	s := NewSampler(reg, time.Second, discardLogger(), Sources{})
	now := time.Unix(1700000000, 0)
	_ = s.sampleAt(now)
	smp := s.sampleAt(now.Add(time.Second))
	if smp.RetransRate != 0 {
		t.Fatalf("无发送段时重传率应为 0，实际 %v", smp.RetransRate)
	}
}

func TestSamplerRunLogsAndStops(t *testing.T) {
	reg := &Registry{}
	reg.SessionsActive.Add(2)
	reg.StreamsActive.Add(5)

	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	s := NewSampler(reg, 20*time.Millisecond, logger, Sources{
		Transport: func() TransportStats { return TransportStats{UDPBytesSent: 1024, OutSegs: 10} },
		Pool:      func() PoolStats { return PoolStats{Sessions: 2, InUse: 1, Idle: 1, Waiters: 3, Rebuilds: 7} },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(buf.String(), "event=metrics") >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后退出")
	}

	out := buf.String()
	if !strings.Contains(out, "event=metrics") {
		t.Fatalf("未见统计日志: %s", out)
	}
	for _, want := range []string{"sessions=2", "streams=5", "payload_sent=", "wire_sent=", "retrans=",
		"payload_recv_total=", "wire_recv_total=", "fec_recovered=", "fec_errs=",
		"pool_in_use=1", "pool_idle=1", "pool_waiters=3", "pool_rebuilds=7", "auth=0 session="} {
		if !strings.Contains(out, want) {
			t.Fatalf("日志缺少 %q: %s", want, out)
		}
	}
}

func TestSamplerRunDisabled(t *testing.T) {
	s := NewSampler(nil, 0, discardLogger(), Sources{})
	done := make(chan struct{})
	go func() {
		s.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("interval=0 时 Run 应立即返回")
	}
	// nil 依赖也要能安全地采样与打日志。
	s.Log(s.Sample())
}

func TestRegistryConcurrentUpdates(t *testing.T) {
	reg := &Registry{}
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.StreamsActive.Add(1)
			reg.StreamsTotal.Add(1)
			reg.PayloadSent.Add(10)
			reg.PayloadReceived.Add(20)
		}()
	}
	wg.Wait()
	if reg.StreamsActive.Load() != n || reg.StreamsTotal.Load() != n {
		t.Fatalf("流计数不对: %d/%d", reg.StreamsActive.Load(), reg.StreamsTotal.Load())
	}
	if reg.PayloadSent.Load() != n*10 || reg.PayloadReceived.Load() != n*20 {
		t.Fatalf("载荷字节不对: %d/%d", reg.PayloadSent.Load(), reg.PayloadReceived.Load())
	}
}
