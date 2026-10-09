package dashboard

import (
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/metrics"
)

func uptr(v uint64) *uint64 { return &v }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// samplePayload 造一份形状固定的上游 /metrics 数据（数值只用于断言，不追求真实）。
func samplePayload(now time.Time) metrics.Payload {
	return metrics.Payload{
		SchemaVersion:     metrics.SchemaVersion,
		Role:              "client",
		StartedAt:         now.Add(-time.Hour),
		UptimeSeconds:     3600,
		Now:               now,
		RateWindowSeconds: 1,
		RateAvailable:     true,
		Sessions:          metrics.CountPair{Active: 2, Total: 12},
		Streams:           metrics.CountPair{Active: 1, Total: 418},
		Payload: metrics.IORates{
			SentTotal: 27 << 20,
			RecvTotal: 142 << 10,
			SentBps:   ptr(446545.9),
			RecvBps:   ptr(2354.1),
		},
		Wire: metrics.IORates{
			SentTotal: 57 << 20,
			RecvTotal: 1007 << 10,
			SentBps:   ptr(2831155.2),
			RecvBps:   ptr(38912.0),
		},
		Link: metrics.LinkPayload{
			OutSegsTotal:         12345,
			InSegsTotal:          12000,
			RetransSegsTotal:     2000,
			FastRetransSegsTotal: 10,
			Window: metrics.LinkWindow{
				Seconds:               1,
				OutSegs:               uptr(320),
				RetransRatio:          ptr(0.5254),
				RetransRatioAvailable: true,
				LostSegs:              uptr(402),
				RepeatSegs:            uptr(7),
				KCPInErrors:           uptr(0),
				FECRecovered:          uptr(35),
				FECErrs:               uptr(0),
			},
		},
		Pool:   metrics.PoolPayload{Available: true, InUse: 1, Idle: 1, Rebuilds: 7},
		Errors: metrics.ErrorCounters{Dial: 3},
		Alarms: []string{"pool_waiters=1"},
	}
}

func addSample(t *testing.T, st *State, at time.Time, mutate func(*metrics.Payload)) Sample {
	t.Helper()
	p := samplePayload(at)
	if mutate != nil {
		mutate(&p)
	}
	st.MarkSuccess(Sample{At: at, Received: at, Metrics: p}, "")
	snap := st.Snapshot()
	if len(snap.Samples) == 0 {
		t.Fatal("样本未写入")
	}
	return snap.Samples[len(snap.Samples)-1]
}

func TestStateMarkSuccessAndFailure(t *testing.T) {
	now := time.Unix(1700000000, 0)
	st := NewState(5)

	if snap := st.Snapshot(); snap.HasData() || snap.Reachable {
		t.Fatalf("初始状态应为空且不可达: %+v", snap)
	}

	addSample(t, st, now, nil)
	snap := st.Snapshot()
	if !snap.Reachable || !snap.HasData() || snap.Role != "client" {
		t.Fatalf("成功拉取后状态不对: %+v", snap)
	}
	if !snap.LastSuccess.Equal(now) || !snap.LastAttempt.Equal(now) || snap.LastError != "" {
		t.Fatalf("时间与错误字段不对: %+v", snap)
	}
	if stale, ok := snap.Stale(now.Add(3 * time.Second)); !ok || stale != 3*time.Second {
		t.Fatalf("陈旧度 = %v（ok=%v）", stale, ok)
	}

	// 失败：保留样本与最近成功时间，只把可达性与错误信息更新掉。
	st.MarkFailure(now.Add(5*time.Second), "dial tcp: connection refused")
	snap = st.Snapshot()
	if snap.Reachable || snap.LastError == "" || len(snap.Samples) != 1 {
		t.Fatalf("失败后应保留数据并标记不可达: %+v", snap)
	}
	if !snap.LastSuccess.Equal(now) || !snap.LastAttempt.Equal(now.Add(5*time.Second)) {
		t.Fatalf("失败不应覆盖最近成功时间: %+v", snap)
	}
}

func TestStateRingCapacity(t *testing.T) {
	base := time.Unix(1700000000, 0)
	st := NewState(3)
	for i := 0; i < 6; i++ {
		addSample(t, st, base.Add(time.Duration(i)*time.Second), nil)
	}
	snap := st.Snapshot()
	if len(snap.Samples) != 3 {
		t.Fatalf("样本数 = %d，期望 3", len(snap.Samples))
	}
	if got := snap.Samples[0].Received; !got.Equal(base.Add(3 * time.Second)) {
		t.Fatalf("环应保留最新 3 条，最旧一条是 %s", got)
	}
}

func TestStateComputesWindowDeltas(t *testing.T) {
	base := time.Unix(1700000000, 0)
	st := NewState(10)
	addSample(t, st, base, nil)
	second := addSample(t, st, base.Add(time.Second), func(p *metrics.Payload) {
		p.Streams.Total = 418 + 100
		p.Errors.Dial = 3 + 2 // 累计错误 3 → 5
	})
	if second.StreamsDelta != 100 || second.ErrorsDelta != 2 {
		t.Fatalf("窗口增量不对: streams=%d errors=%d", second.StreamsDelta, second.ErrorsDelta)
	}
	if got := second.ErrorsPer1k(); got == nil || math.Abs(*got-20) > 1e-9 {
		t.Fatalf("每千条流错误数 = %v，期望 20", got)
	}

	// 窗口内没有新流：曲线断开（nil），而不是 0。
	third := addSample(t, st, base.Add(2*time.Second), func(p *metrics.Payload) {
		p.Streams.Total = 518
		p.Errors.Dial = 5
	})
	if third.ErrorsPer1k() != nil {
		t.Fatal("窗口内没有新流时应返回 nil")
	}
}

func TestStateHandlesUpstreamCounterReset(t *testing.T) {
	base := time.Unix(1700000000, 0)
	st := NewState(10)
	addSample(t, st, base, func(p *metrics.Payload) {
		p.Streams.Total = 5000
		p.Errors.Auth = 100
	})
	// 上游重启：累计量回退，增量应记 0 而不是负数（uint64 回绕）。
	got := addSample(t, st, base.Add(time.Second), func(p *metrics.Payload) {
		p.Streams.Total = 1
		p.Errors.Auth = 0
	})
	if got.StreamsDelta != 0 || got.ErrorsDelta != 0 {
		t.Fatalf("计数器回退时应记 0: streams=%d errors=%d", got.StreamsDelta, got.ErrorsDelta)
	}
}

func TestSnapshotDerived(t *testing.T) {
	now := time.Unix(1700000000, 0)
	st := NewState(10)
	addSample(t, st, now, nil)
	d := st.Snapshot().Derived(now)

	if d.WirePayloadRatio == nil {
		t.Fatal("链路开销比不应为 nil")
	}
	wantRatio := float64(57<<20+1007<<10) / float64(27<<20+142<<10)
	if math.Abs(*d.WirePayloadRatio-wantRatio) > 1e-9 {
		t.Fatalf("链路开销比 = %v，期望 %v", *d.WirePayloadRatio, wantRatio)
	}
	if d.AvgPayloadBps == nil || *d.AvgPayloadBps <= 0 {
		t.Fatalf("平均载荷速率 = %v", d.AvgPayloadBps)
	}
	if d.StreamsPerSession == nil || math.Abs(*d.StreamsPerSession-418.0/12) > 1e-9 {
		t.Fatalf("每会话平均流数 = %v", d.StreamsPerSession)
	}
	if d.ErrorsPer1kStreams == nil || math.Abs(*d.ErrorsPer1kStreams-3.0/418*1000) > 1e-9 {
		t.Fatalf("错误密度 = %v", d.ErrorsPer1kStreams)
	}
	if d.StaleSeconds == nil || *d.StaleSeconds != 0 {
		t.Fatalf("陈旧度 = %v", d.StaleSeconds)
	}
}

func TestSnapshotDerivedEmptyAndZeroSafe(t *testing.T) {
	now := time.Unix(1700000000, 0)
	if d := NewState(4).Snapshot().Derived(now); d != (Derived{}) {
		t.Fatalf("没有样本时派生指标应全为 nil: %+v", d)
	}

	// 全 0 的上游数据：所有比值都应为 nil，页面不会出现 NaN/Inf。
	st := NewState(4)
	st.MarkSuccess(Sample{Received: now, Metrics: metrics.Payload{SchemaVersion: metrics.SchemaVersion}}, "")
	d := st.Snapshot().Derived(now)
	if d.WirePayloadRatio != nil || d.AvgPayloadBps != nil || d.StreamsPerSession != nil || d.ErrorsPer1kStreams != nil {
		t.Fatalf("除零时应为 nil: %+v", d)
	}
}

func TestSampleRetransRatio(t *testing.T) {
	now := time.Unix(1700000000, 0)
	p := samplePayload(now)
	s := Sample{Metrics: p}
	if got := s.RetransRatio(); got == nil || *got != 0.5254 {
		t.Fatalf("重传率 = %v", got)
	}
	p.Link.Window.RetransRatioAvailable = false
	if got := (Sample{Metrics: p}).RetransRatio(); got != nil {
		t.Fatalf("上游标记不可用时应为 nil，实际 %v", *got)
	}
}
