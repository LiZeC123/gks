package dashboard

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/metrics"
)

func TestNewPageChartsAndDerived(t *testing.T) {
	now := time.Unix(1700000000, 0)
	st := NewState(10)
	for i := 0; i < 3; i++ {
		i := i
		addSample(t, st, now.Add(time.Duration(i)*time.Second), func(p *metrics.Payload) {
			p.Streams.Total = 418 + uint64(i)*10
		})
	}
	page := NewPage(st.Snapshot(), PageOptions{
		PullURL:  "http://127.0.0.1:12081/metrics",
		Listen:   "0.0.0.0:12080",
		Interval: time.Second,
		Refresh:  2 * time.Second,
		Capacity: 10,
	}, now.Add(3*time.Second))

	if !page.HasData || !page.Reachable || page.SampleCount != 3 {
		t.Fatalf("页面基本状态不对: hasData=%v reachable=%v samples=%d", page.HasData, page.Reachable, page.SampleCount)
	}
	if page.RefreshMS != 2000 {
		t.Fatalf("RefreshMS = %d", page.RefreshMS)
	}
	if page.TotalErrors != 3 {
		t.Fatalf("错误累计 = %d，期望 3", page.TotalErrors)
	}
	if page.Role != "client" || page.M.Payload.SentTotal != 27<<20 {
		t.Fatalf("上游数据未透传: role=%q payload=%+v", page.Role, page.M.Payload)
	}
	if len(page.Charts) != 4 {
		t.Fatalf("应有 4 张折线图，实际 %d", len(page.Charts))
	}
	wantTitles := []string{"上行", "下行", "重传率", "错误率"}
	for i, want := range wantTitles {
		if !strings.Contains(page.Charts[i].Title, want) {
			t.Fatalf("第 %d 张图标题 = %q，应包含 %q", i, page.Charts[i].Title, want)
		}
		if !strings.Contains(string(page.Charts[i].SVG), "<svg") {
			t.Fatalf("第 %d 张图没有 SVG", i)
		}
		if len(page.Charts[i].Legend) == 0 {
			t.Fatalf("第 %d 张图没有图例", i)
		}
	}
	if len(page.Charts[0].Legend) != 2 || len(page.Charts[1].Legend) != 2 {
		t.Fatalf("上行/下行应为「载荷 + 线」两条线: %d/%d", len(page.Charts[0].Legend), len(page.Charts[1].Legend))
	}
	if len(page.Charts[2].Legend) != 1 || len(page.Charts[3].Legend) != 1 {
		t.Fatal("重传率与错误率各应为一条线")
	}
	if !strings.Contains(page.Charts[0].Note, "线/载荷") {
		t.Fatalf("上行图注释应给出线/载荷比值: %q", page.Charts[0].Note)
	}
	// 面板告警：重传率 52.54% 且窗口发送段 320 ≥ 20。
	if len(page.Alarms) != 1 || !strings.Contains(page.Alarms[0], "retrans=52.54%") {
		t.Fatalf("面板告警 = %v", page.Alarms)
	}
	// 上游 alarms 原样转发。
	if len(page.UpstreamAlarms) != 1 || page.UpstreamAlarms[0] != "pool_waiters=1" {
		t.Fatalf("上游告警 = %v", page.UpstreamAlarms)
	}
	if page.Derived.WirePayloadRatio == nil || page.Derived.ErrorsPer1kStreams == nil {
		t.Fatalf("派生指标缺失: %+v", page.Derived)
	}
	if page.StaleText == "" {
		t.Fatal("应有陈旧度文本")
	}
}

func TestNewPageWithoutData(t *testing.T) {
	now := time.Unix(1700000000, 0)
	st := NewState(10)
	st.MarkFailure(now, "dial tcp 127.0.0.1:12081: connect: connection refused")

	page := NewPage(st.Snapshot(), PageOptions{Interval: time.Second, Capacity: 10}, now)
	if page.HasData || page.Reachable {
		t.Fatalf("无数据时状态不对: hasData=%v reachable=%v", page.HasData, page.Reachable)
	}
	if len(page.Charts) != 0 {
		t.Fatalf("无数据时不应有图: %d", len(page.Charts))
	}
	if len(page.Alarms) != 1 || !strings.Contains(page.Alarms[0], "尚未") {
		t.Fatalf("应提示尚未取到数据: %v", page.Alarms)
	}
	if !strings.Contains(page.LastError, "connection refused") {
		t.Fatalf("错误信息未透传: %q", page.LastError)
	}
	if page.StaleText != "" {
		t.Fatalf("从未成功时不应有陈旧度文本: %q", page.StaleText)
	}
}

func TestPanelAlarmsStalePoolWaiters(t *testing.T) {
	now := time.Unix(1700000000, 0)
	st := NewState(10)
	// 成功过一次，但页面在 10 秒后才渲染（间隔 1s → 超过 3 倍算陈旧）。
	st.MarkSuccess(Sample{Received: now, Metrics: metrics.Payload{
		SchemaVersion: metrics.SchemaVersion,
		Pool:          metrics.PoolPayload{Available: true, Waiters: 2},
	}}, "")
	page := NewPage(st.Snapshot(), PageOptions{Interval: time.Second, Capacity: 10}, now.Add(10*time.Second))

	joined := strings.Join(page.Alarms, " | ")
	if !strings.Contains(joined, "陈旧") {
		t.Fatalf("应报数据陈旧: %v", page.Alarms)
	}
	if !strings.Contains(joined, "pool_waiters=2") {
		t.Fatalf("应报 pool_waiters: %v", page.Alarms)
	}

	// 上游明确不可达时优先报不可达。
	st.MarkFailure(now.Add(11*time.Second), "boom")
	page = NewPage(st.Snapshot(), PageOptions{Interval: time.Second, Capacity: 10}, now.Add(11*time.Second))
	if joined := strings.Join(page.Alarms, " | "); !strings.Contains(joined, "上游不可达") {
		t.Fatalf("应报上游不可达: %v", page.Alarms)
	}
}

func TestOverheadNote(t *testing.T) {
	if got := overheadNote("上行", nil, ptr(1)); !strings.Contains(got, "越贴近") {
		t.Fatalf("缺数据时应给通用提示: %q", got)
	}
	if got := overheadNote("上行", ptr(0), ptr(1)); !strings.Contains(got, "越贴近") {
		t.Fatalf("载荷为 0 时不应做除法: %q", got)
	}
	got := overheadNote("上行", ptr(1000), ptr(1500))
	if !strings.Contains(got, "1.500") || !strings.Contains(got, "50.0%") {
		t.Fatalf("应给出比值与开销: %q", got)
	}
}

func TestTemplateFuncs(t *testing.T) {
	fns := templateFuncs()
	if got := fns["humanBytes"].(func(uint64) string)(27 << 20); got != "27.0 MB" {
		t.Fatalf("humanBytes = %q", got)
	}
	if got := fns["humanRate"].(func(*float64) string)(nil); got != "—" {
		t.Fatalf("nil 速率应显示占位符，实际 %q", got)
	}
	if got := fns["humanRate"].(func(*float64) string)(ptr(2048)); got != "2.0 KB/s" {
		t.Fatalf("humanRate = %q", got)
	}
	if got := fns["pct"].(func(*float64) string)(ptr(0.5254)); got != "52.54%" {
		t.Fatalf("pct = %q", got)
	}
	if got := fns["ratio3"].(func(*float64) string)(ptr(1.0245)); got != "1.024" {
		t.Fatalf("ratio3 = %q", got)
	}
	if got := fns["countPtr"].(func(*uint64) string)(nil); got != "n/a" {
		t.Fatalf("countPtr(nil) = %q", got)
	}
	if got := fns["countPtr"].(func(*uint64) string)(uptr(320)); got != "320" {
		t.Fatalf("countPtr = %q", got)
	}
	if got := fns["int64v"].(func(int64) string)(-2); got != "-2" {
		t.Fatalf("int64v = %q", got)
	}
	if got := fns["intv"].(func(int) string)(7); got != "7" {
		t.Fatalf("intv = %q", got)
	}
	if got := fns["uptime"].(func(float64) string)(3723); got != "1h02m03s" {
		t.Fatalf("uptime = %q", got)
	}
	staleFn := fns["stale"].(func(*float64) string)
	if got := staleFn(nil); got != "—" {
		t.Fatalf("stale(nil) = %q", got)
	}
	if got := staleFn(ptr(65)); got != "1m05s" {
		t.Fatalf("stale = %q", got)
	}
	if got := fns["ts"].(func(time.Time) string)(time.Time{}); got != "—" {
		t.Fatalf("零值时间应显示占位符: %q", got)
	}
	if got := fns["ts"].(func(time.Time) string)(time.Unix(1700000000, 0)); !strings.Contains(got, "2023-11-15") && !strings.Contains(got, "2023-11-14") {
		t.Fatalf("时间格式异常: %q", got)
	}
	if got := fns["dursec"].(func(float64) string)(1); got != "1.00s" {
		t.Fatalf("dursec = %q", got)
	}
}

func TestDerivedRatioGuards(t *testing.T) {
	// ratio() 在分母为 0 时返回 nil，页面不会出现 Inf/NaN。
	if got := ratio(1, 0); got != nil {
		t.Fatalf("ratio(1,0) = %v", *got)
	}
	if got := ratio(3, 2); got == nil || math.Abs(*got-1.5) > 1e-9 {
		t.Fatalf("ratio(3,2) = %v", got)
	}
}
