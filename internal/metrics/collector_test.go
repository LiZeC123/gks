package metrics

import (
	"context"
	"testing"
	"time"
)

// snapshotAt 造一条指定时刻的快照，便于构造历史。
func testSnapshot(t *testing.T, reg *Registry, ts TransportStats, pool PoolStats, at time.Time) Snapshot {
	t.Helper()
	return reg.Take(ts, pool, at)
}

func TestCollectorIntervalAndPoolAvailability(t *testing.T) {
	c := NewCollector(Options{Interval: 7 * time.Second, Sources: Sources{Pool: func() PoolStats { return PoolStats{} }}})
	if c.Interval() != 7*time.Second {
		t.Fatalf("Interval = %s", c.Interval())
	}
	if !c.PoolAvailable() {
		t.Fatal("传了 Pool 来源时应报告可用")
	}
	if NewCollector(Options{}).PoolAvailable() {
		t.Fatal("没有 Pool 来源时应报告不可用")
	}
}

func TestCollectorViewWithoutHistory(t *testing.T) {
	reg := &Registry{}
	reg.SessionsActive.Add(2)
	c := NewCollector(Options{Registry: reg, Interval: 10 * time.Second})

	v := c.View(10 * time.Second)
	if v.HasRates {
		t.Fatal("没有历史时不应有速率")
	}
	if v.Now.Registry.SessionsActive != 2 {
		t.Fatalf("新鲜快照应包含当时计数: %+v", v.Now.Registry)
	}
	if v.UserWindow != 10*time.Second {
		t.Fatalf("窗口 = %s", v.UserWindow)
	}
}

func TestCollectorViewComputesWindowRates(t *testing.T) {
	reg := &Registry{}
	cur := TransportStats{UDPBytesSent: 1000, OutSegs: 10}
	pool := PoolStats{InUse: 1, Waiters: 2}
	c := NewCollector(Options{
		Registry: reg,
		Interval: 10 * time.Second,
		Sources: Sources{
			Transport: func() TransportStats { return cur },
			Pool:      func() PoolStats { return pool },
		},
	})

	// 用 15 秒前的样本做窗口起点（窗口 10s 要求起点足够老）。
	base := time.Now().Add(-15 * time.Second)
	c.Record(testSnapshot(t, reg, TransportStats{}, PoolStats{}, base))

	// 窗口起点必须是 base，因此 1000B/1s 的速率会按实际窗口（约 15s）摊薄。
	v := c.View(10 * time.Second)
	if !v.HasRates {
		t.Fatal("有足够历史时应能算出速率")
	}
	if v.Window <= 0 || v.Window > 16*time.Second {
		t.Fatalf("窗口长度不合理: %s", v.Window)
	}
	if want := float64(1000) / v.Window.Seconds(); v.Sample.WireSentBps != want {
		t.Fatalf("线速率 = %v，期望 %v", v.Sample.WireSentBps, want)
	}
	if v.Sample.OutSegsDelta != 10 {
		t.Fatalf("发送段增量 = %d", v.Sample.OutSegsDelta)
	}
	if v.Now.Pool != pool || v.Sample.Current.Pool != pool {
		t.Fatalf("池状态未取到: %+v", v.Now.Pool)
	}
}

func TestCollectorViewWindowTooLongForHistory(t *testing.T) {
	reg := &Registry{}
	c := NewCollector(Options{Registry: reg, Interval: 30 * time.Second})
	c.Record(testSnapshot(t, reg, TransportStats{}, PoolStats{}, time.Now().Add(-2*time.Second)))
	if v := c.View(30 * time.Second); v.HasRates {
		t.Fatal("历史不足一个窗口时不应给出速率")
	}
}

func TestClampWindow(t *testing.T) {
	if got := clampWindow(0); got != Resolution {
		t.Fatalf("过小窗口应夹到 %s，实际 %s", Resolution, got)
	}
	if got := clampWindow(500 * time.Millisecond); got != Resolution {
		t.Fatalf("过小窗口应夹到 %s，实际 %s", Resolution, got)
	}
	if got := clampWindow(time.Hour); got != Retention {
		t.Fatalf("过大窗口应夹到 %s，实际 %s", Retention, got)
	}
	if got := clampWindow(5 * time.Second); got != 5*time.Second {
		t.Fatalf("合法窗口不应被改: %s", got)
	}
}

func TestCollectorHistoryRetentionAndCompaction(t *testing.T) {
	reg := &Registry{}
	c := NewCollector(Options{Registry: reg, Interval: time.Second})
	now := time.Now()
	base := now.Add(-40 * time.Minute)

	// 灌入 40 分钟、每分钟一条的历史：早于保留时长的部分应被丢弃。
	for i := 0; i <= 40; i++ {
		c.Record(testSnapshot(t, reg, TransportStats{}, PoolStats{}, base.Add(time.Duration(i)*time.Minute)))
	}
	latest, ok := c.Latest()
	if !ok {
		t.Fatal("应有最近样本")
	}
	// 保留时长 10 分钟 → 最多留下 11 条（含边界）。
	if c.HistoryLen() == 0 || c.HistoryLen() > int(Retention/time.Minute)+2 {
		t.Fatalf("历史未按保留时长裁剪: %d 条", c.HistoryLen())
	}
	if !latest.At.Equal(now) {
		t.Fatalf("最近样本时间不对: %s", latest.At)
	}
}

func TestCollectorLookupBoundaries(t *testing.T) {
	reg := &Registry{}
	c := NewCollector(Options{Registry: reg, Interval: time.Second})
	base := time.Unix(1700000000, 0)
	c.Record(testSnapshot(t, reg, TransportStats{}, PoolStats{}, base))
	c.Record(testSnapshot(t, reg, TransportStats{}, PoolStats{}, base.Add(10*time.Second)))

	// 窗口 10s：目标时刻 base+0s，正好命中第一条。
	got, ok := c.lookup(base.Add(10*time.Second), 10*time.Second)
	if !ok || !got.At.Equal(base) {
		t.Fatalf("应命中 base 的样本: ok=%v at=%s", ok, got.At)
	}
	// 窗口 20s：目标时刻 base-10s，没有任何样本足够老。
	if _, ok := c.lookup(base.Add(10*time.Second), 20*time.Second); ok {
		t.Fatal("历史不足时不应命中")
	}
}

func TestCollectorRunRecordsHistoryAndStops(t *testing.T) {
	reg := &Registry{}
	reg.SessionsActive.Add(1)
	c := NewCollector(Options{Registry: reg, Interval: 10 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && c.HistoryLen() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后退出")
	}
	if c.HistoryLen() < 2 {
		t.Fatalf("Run 应至少记录基准 + 一次采样，实际 %d 条", c.HistoryLen())
	}
}

// TestCollectorRunSilentAboutRates 只是记录：采集器本身不产生任何日志/渲染副作用，
// 因此不需要 logger；这里断言它没有日志依赖（编译期即可证明）。
func TestCollectorUsesDefaultRegistry(t *testing.T) {
	c := NewCollector(Options{Interval: time.Second})
	if c.reg != Default {
		t.Fatal("未指定 Registry 时应使用 Default")
	}
}
