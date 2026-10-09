package dashboard

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/LiZeC123/gks/internal/metrics"
)

// ValueKind 决定 Y 轴的取值格式化方式与刻度下限。
type ValueKind int

const (
	// KindBytesPerSec 是速率（B/s），用 metrics.HumanRate 格式化。
	KindBytesPerSec ValueKind = iota
	// KindPercent 是百分比（上游给的比例 ×100 之前的小数，例如 0.5254 表示 52.54%）。
	KindPercent
	// KindPer1k 是「每千条流的次数」这类小数值。
	KindPer1k
)

// ChartSeries 是一条折线；Values 与 ChartSpec.Times 一一对应，nil 表示该点缺失（断开）。
type ChartSeries struct {
	Name   string
	Color  string
	Values []*float64
}

// ChartSpec 是一张折线图的完整描述。
type ChartSpec struct {
	Title  string
	Kind   ValueKind
	Series []ChartSeries
	Times  []time.Time
}

// SVG 画布与留白（viewBox 坐标，页面上用 CSS 拉伸到容器宽度）。
const (
	chartWidth  = 960
	chartHeight = 220
	padLeft     = 84
	padRight    = 18
	padTop      = 14
	padBottom   = 30
	gridLines   = 4
)

// RenderChartSVG 把一条或多条折线渲染成内联 SVG 字符串。
//
// 纯函数：同样的输入产生同样的输出，便于单测；输出只由自己生成的数字与固定颜色组成，
// 因此模板侧可以安全地当作 template.HTML 使用。
func RenderChartSVG(spec ChartSpec) string {
	n := seriesLength(spec.Series)
	plotW := chartWidth - padLeft - padRight
	plotH := chartHeight - padTop - padBottom

	maxV, hasValue := maxValue(spec.Series)
	if !hasValue {
		return fmt.Sprintf(
			`<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="%s"><text class="chart-empty" x="%d" y="%d">暂无数据</text></svg>`,
			chartWidth, chartHeight, escapeXML(spec.Title), chartWidth/2, chartHeight/2)
	}
	top := axisTop(maxV, spec.Kind)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="%s">`, chartWidth, chartHeight, escapeXML(spec.Title))

	// 横向网格 + Y 轴刻度
	for k := 0; k <= gridLines; k++ {
		y := float64(padTop) + float64(plotH)*float64(k)/gridLines
		value := top * (1 - float64(k)/gridLines)
		fmt.Fprintf(&b, `<line class="grid" x1="%d" y1="%.1f" x2="%d" y2="%.1f"/>`,
			padLeft, y, chartWidth-padRight, y)
		fmt.Fprintf(&b, `<text class="axis-y" x="%d" y="%.1f">%s</text>`,
			padLeft-8, y+4, escapeXML(formatValue(value, spec.Kind)))
	}
	// 坐标轴
	fmt.Fprintf(&b, `<line class="axis" x1="%d" y1="%d" x2="%d" y2="%d"/>`,
		padLeft, padTop, padLeft, padTop+plotH)
	fmt.Fprintf(&b, `<line class="axis" x1="%d" y1="%d" x2="%d" y2="%d"/>`,
		padLeft, padTop+plotH, chartWidth-padRight, padTop+plotH)

	// 折线：遇到 nil 就断开
	for _, series := range spec.Series {
		for _, seg := range segments(series.Values) {
			if len(seg) == 1 {
				x, y := pointXY(seg[0], *series.Values[seg[0]], n, plotW, plotH, top)
				fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="2.5" fill="%s"/>`, x, y, series.Color)
				continue
			}
			pts := make([]string, 0, len(seg))
			for _, idx := range seg {
				x, y := pointXY(idx, *series.Values[idx], n, plotW, plotH, top)
				pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
			}
			fmt.Fprintf(&b, `<polyline class="series" stroke="%s" points="%s"/>`, series.Color, strings.Join(pts, " "))
		}
	}

	// X 轴两端时间标签
	if len(spec.Times) > 0 {
		fmt.Fprintf(&b, `<text class="axis-x" x="%d" y="%d">%s</text>`,
			padLeft, chartHeight-10, spec.Times[0].Format("15:04:05"))
		fmt.Fprintf(&b, `<text class="axis-x end" x="%d" y="%d">%s</text>`,
			chartWidth-padRight, chartHeight-10, spec.Times[len(spec.Times)-1].Format("15:04:05"))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// axisTop 返回 Y 轴上限：留 10% 余量，并给百分比/小数图一个 1 的下限，
// 避免全 0 或极小值时曲线贴边看不出变化。
func axisTop(maxV float64, kind ValueKind) float64 {
	if kind != KindBytesPerSec && maxV < 1 {
		maxV = 1
	}
	if maxV <= 0 {
		maxV = 1
	}
	return maxV * 1.1
}

func maxValue(series []ChartSeries) (float64, bool) {
	maxV := 0.0
	has := false
	for _, s := range series {
		for _, v := range s.Values {
			if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
				continue
			}
			if *v > maxV {
				maxV = *v
			}
			has = true
		}
	}
	return maxV, has
}

func seriesLength(series []ChartSeries) int {
	n := 0
	for _, s := range series {
		if len(s.Values) > n {
			n = len(s.Values)
		}
	}
	return n
}

// segments 把值序列切成若干连续（非 nil）的下标段。
func segments(values []*float64) [][]int {
	var out [][]int
	var cur []int
	for i, v := range values {
		if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
			if len(cur) > 0 {
				out = append(out, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, i)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func pointXY(i int, value float64, n, plotW, plotH int, top float64) (float64, float64) {
	var x float64
	if n <= 1 {
		x = float64(padLeft) + float64(plotW)/2
	} else {
		x = float64(padLeft) + float64(i)*float64(plotW)/float64(n-1)
	}
	y := float64(padTop) + float64(plotH)*(1-value/top)
	return x, y
}

// formatValue 按图表类型格式化 Y 轴刻度。
func formatValue(v float64, kind ValueKind) string {
	switch kind {
	case KindPercent:
		return fmt.Sprintf("%.2f%%", v*100)
	case KindPer1k:
		return fmt.Sprintf("%.2f", v)
	default:
		return metrics.HumanRate(v)
	}
}

// escapeXML 转义放进 SVG 文本/属性里的字符串（标题固定由我们提供，这里只做兜底）。
func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
