package dashboard

import (
	"fmt"
	"html/template"
	"strconv"
	"time"

	"github.com/LiZeC123/gks/internal/metrics"
)

// 折线配色：上行（载荷蓝 / 线红）、下行（载荷绿 / 线橙）、重传紫、错误红。
const (
	colorPayloadUp   = "#2563eb"
	colorWireUp      = "#dc2626"
	colorPayloadDown = "#059669"
	colorWireDown    = "#d97706"
	colorRetrans     = "#7c3aed"
	colorErrors      = "#be123c"
)

// staleFactor 决定「多久没拉到数据算陈旧」：拉取间隔的 3 倍。
const staleFactor = 3

// PageOptions 是面板展示层的静态参数（启动后不变）。
type PageOptions struct {
	// PullURL 是上游 /metrics 地址。
	PullURL string
	// Listen 是面板自己的监听地址（仅用于展示）。
	Listen string
	// Interval 是拉取间隔。
	Interval time.Duration
	// Refresh 是页面局部刷新间隔；0 表示只显示首屏。
	Refresh time.Duration
	// Capacity 是样本环容量。
	Capacity int
}

// Page 是页面模板的数据。
type Page struct {
	GeneratedAt time.Time
	PullURL     string
	Listen      string
	Interval    time.Duration
	// RefreshMS 是局部刷新间隔（毫秒）；<=0 表示不自动刷新。
	RefreshMS   int
	Capacity    int
	SampleCount int

	HasData     bool
	Reachable   bool
	Role        string
	SchemaWarn  string
	LastError   string
	LastAttempt time.Time
	LastSuccess time.Time
	// StaleText 是「距最近成功拉取多久」的可读文本；无成功记录时为空。
	StaleText string

	M              metrics.Payload
	TotalErrors    uint64
	Derived        Derived
	Alarms         []string
	UpstreamAlarms []string
	Charts         []Chart
}

// Chart 是页面上的一张折线图；SVG 由 Go 生成（见 RenderChartSVG）。
type Chart struct {
	Title  string
	Note   string
	Legend []Legend
	SVG    template.HTML
}

// Legend 是图例项。
type Legend struct {
	Name  string
	Color string
}

// NewPage 把一次状态快照渲染成页面数据。
func NewPage(snap Snapshot, o PageOptions, now time.Time) Page {
	p := Page{
		GeneratedAt:    now,
		PullURL:        o.PullURL,
		Listen:         o.Listen,
		RefreshMS:      int(o.Refresh / time.Millisecond),
		Capacity:       snap.Capacity,
		SampleCount:    len(snap.Samples),
		HasData:        snap.HasData(),
		Reachable:      snap.Reachable,
		Role:           snap.Role,
		SchemaWarn:     snap.SchemaWarn,
		LastError:      snap.LastError,
		LastAttempt:    snap.LastAttempt,
		LastSuccess:    snap.LastSuccess,
		Derived:        snap.Derived(now),
		Alarms:         panelAlarms(snap, o, now),
		UpstreamAlarms: upstreamAlarms(snap),
	}
	if stale, ok := snap.Stale(now); ok {
		p.StaleText = metrics.FormatUptime(stale)
	}
	if latest, ok := snap.Latest(); ok {
		p.M = latest.Metrics
		p.TotalErrors = TotalErrors(latest.Metrics)
	}
	p.Charts = buildCharts(snap.Samples)
	return p
}

// panelAlarms 是面板自己根据状态与最新样本判断的告警。
func panelAlarms(snap Snapshot, o PageOptions, now time.Time) []string {
	var out []string
	if !snap.HasData() {
		return append(out, "尚未从上游取到数据")
	}
	stale, hasStale := snap.Stale(now)
	switch {
	case !snap.Reachable:
		if hasStale {
			out = append(out, fmt.Sprintf("上游不可达（最近成功 %s 前）", metrics.FormatUptime(stale)))
		} else {
			out = append(out, "上游不可达")
		}
	case hasStale && o.Interval > 0 && stale > staleFactor*o.Interval:
		out = append(out, fmt.Sprintf("上游数据陈旧（最近成功 %s 前）", metrics.FormatUptime(stale)))
	}
	latest, _ := snap.Latest()
	if latest.Metrics.Pool.Available && latest.Metrics.Pool.Waiters > 0 {
		out = append(out, fmt.Sprintf("pool_waiters=%d（并发已顶到 max_sessions）", latest.Metrics.Pool.Waiters))
	}
	if r := latest.RetransRatio(); r != nil && *r > 0.01 {
		out = append(out, fmt.Sprintf("retrans=%.2f%%（≥1%%）", *r*100))
	}
	return out
}

// upstreamAlarms 转发上游 /metrics 的 alarms 字段。
func upstreamAlarms(snap Snapshot) []string {
	latest, ok := snap.Latest()
	if !ok {
		return nil
	}
	return latest.Metrics.Alarms
}

// buildCharts 由样本环生成四张折线图：
// 上行（载荷 vs 线）、下行（载荷 vs 线）、重传率、错误率（每千条流）。
func buildCharts(samples []Sample) []Chart {
	if len(samples) == 0 {
		return nil
	}
	times := make([]time.Time, len(samples))
	upPayload := make([]*float64, len(samples))
	upWire := make([]*float64, len(samples))
	downPayload := make([]*float64, len(samples))
	downWire := make([]*float64, len(samples))
	retrans := make([]*float64, len(samples))
	errors := make([]*float64, len(samples))

	for i, s := range samples {
		times[i] = s.Received
		upPayload[i] = s.Metrics.Payload.SentBps
		upWire[i] = s.Metrics.Wire.SentBps
		downPayload[i] = s.Metrics.Payload.RecvBps
		downWire[i] = s.Metrics.Wire.RecvBps
		retrans[i] = s.RetransRatio()
		errors[i] = s.ErrorsPer1k()
	}
	latest := samples[len(samples)-1]
	return []Chart{
		newChart("上行速率：载荷 vs 线",
			overheadNote("上行", latest.Metrics.Payload.SentBps, latest.Metrics.Wire.SentBps),
			KindBytesPerSec, times,
			ChartSeries{Name: "载荷上行", Color: colorPayloadUp, Values: upPayload},
			ChartSeries{Name: "线上行（UDP）", Color: colorWireUp, Values: upWire}),
		newChart("下行速率：载荷 vs 线",
			overheadNote("下行", latest.Metrics.Payload.RecvBps, latest.Metrics.Wire.RecvBps),
			KindBytesPerSec, times,
			ChartSeries{Name: "载荷下行", Color: colorPayloadDown, Values: downPayload},
			ChartSeries{Name: "线下行（UDP）", Color: colorWireDown, Values: downWire}),
		newChart("重传率",
			"上游窗口内「重传段 / 发出的段」；发送段少于 20 时上游给 n/a，曲线断开。",
			KindPercent, times,
			ChartSeries{Name: "重传率", Color: colorRetrans, Values: retrans}),
		newChart("错误率（每千条流）",
			"面板用相邻样本的累计错误数差分后按每千条流折算；该窗口内没有新流时曲线断开。",
			KindPer1k, times,
			ChartSeries{Name: "错误/千条流", Color: colorErrors, Values: errors}),
	}
}

func newChart(title, note string, kind ValueKind, times []time.Time, series ...ChartSeries) Chart {
	chart := Chart{Title: title, Note: note}
	for _, s := range series {
		chart.Legend = append(chart.Legend, Legend{Name: s.Name, Color: s.Color})
	}
	chart.SVG = template.HTML(RenderChartSVG(ChartSpec{ //nolint:gosec // 内容由本包自己生成
		Title:  title,
		Kind:   kind,
		Series: series,
		Times:  times,
	}))
	return chart
}

// overheadNote 给出最新窗口的线/载荷比值，让「开销」在图上有个具体数字。
func overheadNote(direction string, payload, wire *float64) string {
	base := direction + "：载荷是应用数据，线是 UDP 字节。"
	if payload == nil || wire == nil || *payload <= 0 {
		return base + "两条线越贴近，链路开销越小。"
	}
	r := *wire / *payload
	return fmt.Sprintf("%s最新窗口 线/载荷 = %.3f（开销 %.1f%%）。", base, r, (r-1)*100)
}

// templateFuncs 是模板里用到的格式化函数。
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"humanBytes": func(v uint64) string { return metrics.HumanBytes(float64(v)) },
		"humanRate": func(v *float64) string {
			if v == nil {
				return "—"
			}
			return metrics.HumanRate(*v)
		},
		"pct": func(v *float64) string {
			if v == nil {
				return "—"
			}
			return fmt.Sprintf("%.2f%%", *v*100)
		},
		"ratio3": func(v *float64) string {
			if v == nil {
				return "—"
			}
			return fmt.Sprintf("%.3f", *v)
		},
		"plain2": func(v *float64) string {
			if v == nil {
				return "—"
			}
			return fmt.Sprintf("%.2f", *v)
		},
		"count": func(v uint64) string { return strconv.FormatUint(v, 10) },
		"countPtr": func(v *uint64) string {
			if v == nil {
				return "n/a"
			}
			return strconv.FormatUint(*v, 10)
		},
		"int64v": func(v int64) string { return strconv.FormatInt(v, 10) },
		"intv":   func(v int) string { return strconv.Itoa(v) },
		"uptime": func(sec float64) string {
			if sec <= 0 {
				return "—"
			}
			return metrics.FormatUptime(time.Duration(sec * float64(time.Second)))
		},
		"stale": func(sec *float64) string {
			if sec == nil {
				return "—"
			}
			return metrics.FormatUptime(time.Duration(*sec * float64(time.Second)))
		},
		"ts": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Format("2006-01-02 15:04:05")
		},
		"clock": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Format("15:04:05")
		},
		"dursec": func(sec float64) string { return fmt.Sprintf("%.2fs", sec) },
	}
}
