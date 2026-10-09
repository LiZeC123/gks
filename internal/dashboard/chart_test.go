package dashboard

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestRenderChartSVGEmpty(t *testing.T) {
	out := RenderChartSVG(ChartSpec{Title: "重传率", Kind: KindPercent})
	if !strings.Contains(out, "暂无数据") {
		t.Fatalf("无数据时应给出提示: %s", out)
	}
	if strings.Contains(out, "NaN") {
		t.Fatalf("不应出现 NaN: %s", out)
	}

	// 全是 nil 的序列同样按无数据处理。
	out = RenderChartSVG(ChartSpec{
		Title:  "重传率",
		Kind:   KindPercent,
		Series: []ChartSeries{{Name: "r", Color: "#000", Values: []*float64{nil, nil}}},
	})
	if !strings.Contains(out, "暂无数据") {
		t.Fatalf("全 nil 时应给出提示: %s", out)
	}
}

func TestRenderChartSVGSeriesAndGaps(t *testing.T) {
	base := time.Unix(1700000000, 0)
	times := []time.Time{base, base.Add(time.Second), base.Add(2 * time.Second), base.Add(3 * time.Second), base.Add(4 * time.Second)}
	out := RenderChartSVG(ChartSpec{
		Title: "上行",
		Kind:  KindBytesPerSec,
		Times: times,
		Series: []ChartSeries{
			{Name: "载荷上行", Color: colorPayloadUp, Values: []*float64{ptr(1000), ptr(2000), nil, ptr(4000), ptr(5000)}},
			{Name: "线上行", Color: colorWireUp, Values: []*float64{ptr(2000), ptr(3000), nil, ptr(6000), ptr(7000)}},
		},
	})
	if got := strings.Count(out, "<polyline"); got != 4 {
		t.Fatalf("两条线各被 nil 断成两段，polyline 数 = %d，期望 4\n%s", got, out)
	}
	if strings.Count(out, `<line class="grid"`) != gridLines+1 {
		t.Fatalf("网格线数量不对:\n%s", out)
	}
	if !strings.Contains(out, colorPayloadUp) || !strings.Contains(out, colorWireUp) {
		t.Fatalf("颜色未使用:\n%s", out)
	}
	if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
		t.Fatalf("坐标不应出现 NaN/Inf:\n%s", out)
	}
	for _, want := range []string{times[0].Format("15:04:05"), times[4].Format("15:04:05")} {
		if !strings.Contains(out, want) {
			t.Fatalf("X 轴时间标签缺少 %s:\n%s", want, out)
		}
	}
}

func TestRenderChartSVGSinglePoint(t *testing.T) {
	out := RenderChartSVG(ChartSpec{
		Title:  "重传率",
		Kind:   KindPercent,
		Times:  []time.Time{time.Unix(1700000000, 0)},
		Series: []ChartSeries{{Name: "重传率", Color: colorRetrans, Values: []*float64{ptr(0.5)}}},
	})
	if !strings.Contains(out, "<circle") {
		t.Fatalf("只有一个点时应当画点而不是折线:\n%s", out)
	}
	if strings.Contains(out, "<polyline") {
		t.Fatalf("单点不应画折线:\n%s", out)
	}
	// Y 轴上限被抬到 1%（axisTop 的小值下限）再留 10% 余量 → 110%/…/0%。
	if !strings.Contains(out, "110.00%") || !strings.Contains(out, "0.00%") {
		t.Fatalf("百分比的 Y 轴刻度应按 %% 格式化:\n%s", out)
	}
}

func TestRenderChartSVGYFormatting(t *testing.T) {
	rate := RenderChartSVG(ChartSpec{
		Title:  "速率",
		Kind:   KindBytesPerSec,
		Series: []ChartSeries{{Name: "s", Color: "#000", Values: []*float64{ptr(1024 * 1024)}}},
	})
	if !strings.Contains(rate, "MB/s") {
		t.Fatalf("速率刻度应使用人类可读单位:\n%s", rate)
	}
	per1k := RenderChartSVG(ChartSpec{
		Title:  "错误率",
		Kind:   KindPer1k,
		Series: []ChartSeries{{Name: "s", Color: "#000", Values: []*float64{ptr(20)}}},
	})
	if !strings.Contains(per1k, "22.00") {
		t.Fatalf("每千条流刻度应保留两位小数（20 × 1.1 上限）:\n%s", per1k)
	}
}

func TestAxisTop(t *testing.T) {
	cases := []struct {
		kind ValueKind
		max  float64
		want float64
	}{
		{KindBytesPerSec, 0, 1.1},     // 全 0：给个默认上限，避免除以 0
		{KindBytesPerSec, 1000, 1100}, // 10% 余量
		{KindPercent, 0.5, 1.1},       // 小于 1% 时下限按 1%
		{KindPercent, 50, 55},         // 大值仍按 10% 余量
		{KindPer1k, 0.2, 1.1},
	}
	for _, tc := range cases {
		if got := axisTop(tc.max, tc.kind); math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("axisTop(%v, %v) = %v，期望 %v", tc.max, tc.kind, got, tc.want)
		}
	}
}

func TestEscapeXML(t *testing.T) {
	got := escapeXML(`<a href="x">&'</a>`)
	want := "&lt;a href=&quot;x&quot;&gt;&amp;&apos;&lt;/a&gt;"
	if got != want {
		t.Fatalf("escapeXML = %q，期望 %q", got, want)
	}
}
