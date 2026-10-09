package metrics

import (
	"sync"
	"testing"
	"time"
)

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

func TestComputeSampleRates(t *testing.T) {
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
	t0 := time.Unix(1700000000, 0)
	prev := reg.Take(transport, PoolStats{}, t0)

	// 2 秒后：载荷 +2048/+1024，UDP +10MB/+5MB，段 +200/+100，重传 +20/丢包 +6/重复 +3
	reg.PayloadSent.Add(2048)
	reg.PayloadReceived.Add(1024)
	cur := reg.Take(TransportStats{
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
	}, PoolStats{}, t0.Add(2*time.Second))

	second := computeSample(prev, cur)

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

func TestComputeSampleZeroInterval(t *testing.T) {
	reg := &Registry{}
	reg.PayloadSent.Add(100)
	cur := TransportStats{UDPBytesSent: 1000, OutSegs: 10}
	now := time.Unix(1700000000, 0)
	prev := reg.Take(cur, PoolStats{}, now)
	reg.PayloadSent.Add(100)
	cur = TransportStats{UDPBytesSent: 2000, OutSegs: 20}

	smp := computeSample(prev, reg.Take(cur, PoolStats{}, now))
	if smp.Interval != 0 || smp.PayloadSentBps != 0 || smp.RetransRate != 0 {
		t.Fatalf("零间隔应无速率: %+v", smp)
	}
}

func TestComputeSampleHandlesCounterReset(t *testing.T) {
	reg := &Registry{}
	reg.PayloadSent.Add(1000)
	cur := TransportStats{UDPBytesSent: 5000, OutSegs: 100, RetransSegs: 5}
	now := time.Unix(1700000000, 0)
	prev := reg.Take(cur, PoolStats{}, now)

	// 计数器回退（例如对端重启导致 Snmp 重置）：增量应为 0 而不是负数。
	reg.PayloadSent.Store(10)
	smp := computeSample(prev, reg.Take(TransportStats{UDPBytesSent: 1, OutSegs: 1}, PoolStats{}, now.Add(time.Second)))
	if smp.PayloadSentBps != 0 || smp.WireSentBps != 0 || smp.RetransRate != 0 {
		t.Fatalf("计数器回退时应为 0: %+v", smp)
	}
}

func TestComputeSampleRetransRateWithoutOutSegs(t *testing.T) {
	reg := &Registry{}
	now := time.Unix(1700000000, 0)
	prev := reg.Take(TransportStats{}, PoolStats{}, now)
	smp := computeSample(prev, reg.Take(TransportStats{}, PoolStats{}, now.Add(time.Second)))
	if smp.RetransRate != 0 {
		t.Fatalf("无发送段时重传率应为 0，实际 %v", smp.RetransRate)
	}
}

func TestRetransText(t *testing.T) {
	if got := retransText(Sample{OutSegsDelta: 10, RetransRate: 0.5}); got != "n/a" {
		t.Fatalf("小样本应显示 n/a，实际 %q", got)
	}
	if got := retransText(Sample{OutSegsDelta: 100, RetransRate: 0.5}); got != "50.00%" {
		t.Fatalf("重传率文本 = %q", got)
	}
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
