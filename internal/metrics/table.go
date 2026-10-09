package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TableOptions 是控制台表格的呈现参数。
type TableOptions struct {
	// Role 是进程角色（client / server）。
	Role string
	// StartedAt 是进程启动时刻，用于计算运行时长。
	StartedAt time.Time
	// Now 是渲染时刻；为空值时用 time.Now()。
	Now time.Time
}

// Alarms 返回需要关注的指标描述（判据见 dev.md §9.5.1）：
// 会话池排队、链路重传率 ≥ 1%（发送段足够时）、FEC 恢复失败、KCP 输入错误。
func Alarms(v View) []string {
	items := []string{}
	if v.HasPool && v.Now.Pool.Waiters > 0 {
		items = append(items, fmt.Sprintf("pool_waiters=%d", v.Now.Pool.Waiters))
	}
	if v.HasRates {
		if v.Sample.OutSegsDelta >= minRetransSampleSegs && v.Sample.RetransRate > 0.01 {
			items = append(items, fmt.Sprintf("retrans=%.2f%%(≥1%%)", v.Sample.RetransRate*100))
		}
		if v.Sample.FECErrsDelta > 0 {
			items = append(items, fmt.Sprintf("fec_errs=%d", v.Sample.FECErrsDelta))
		}
		if v.Sample.KCPInErrorsDelta > 0 {
			items = append(items, fmt.Sprintf("kcp_in_errors=%d", v.Sample.KCPInErrorsDelta))
		}
	}
	return items
}

// tableRow 是表格里的一行：分组、指标名、数值。
type tableRow struct {
	group string
	name  string
	value string
}

// RenderTable 把一次统计呈现渲染成控制台表格。
//
// 输出是纯文本（不含任何 ANSI 转义）：原地刷新所需的控制序列由 monitor 负责，
// 这样「重定向到文件」的场景可以直接留档一份可读的表格。
// 行高固定（始终带一行告警行），便于原地刷新时不残留旧行。
func RenderTable(v View, o TableOptions) string {
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	rows := tableRows(v)
	w1, w2, w3 := columnWidths(rows)

	var b strings.Builder
	fmt.Fprintf(&b, "gks %s · 运行 %s · 窗口 %s · %s\n",
		o.Role, FormatUptime(now.Sub(o.StartedAt)), v.UserWindow, now.Format("2006-01-02 15:04:05"))
	if !v.HasRates {
		b.WriteString("（速率历史不足一个窗口，速率与增量显示 n/a）\n")
	}
	b.WriteString(border("┌", "┬", "┐", w1, w2, w3) + "\n")
	lastGroup := ""
	for _, r := range rows {
		if lastGroup != "" && r.group != lastGroup {
			b.WriteString(border("├", "┼", "┤", w1, w2, w3) + "\n")
		}
		lastGroup = r.group
		fmt.Fprintf(&b, "│ %s │ %s │ %s │\n", pad(r.group, w1), pad(r.name, w2), pad(r.value, w3))
	}
	b.WriteString(border("└", "┴", "┘", w1, w2, w3) + "\n")
	if alarms := Alarms(v); len(alarms) > 0 {
		b.WriteString("[告警] " + strings.Join(alarms, "；") + "\n")
	} else {
		b.WriteString("[告警] 无\n")
	}
	return b.String()
}

// tableRows 生成固定顺序的分组行：会话/流 → 载荷速率 → 线速率 → 累计量 →
// 链路质量 → 会话池（仅客户端）→ 错误。
func tableRows(v View) []tableRow {
	r := v.Now.Registry
	rows := []tableRow{
		{"会话/流", "sessions", strconv.FormatInt(r.SessionsActive, 10)},
		{"会话/流", "streams", strconv.FormatInt(r.StreamsActive, 10)},
		{"会话/流", "sessions_total", strconv.FormatUint(r.SessionsTotal, 10)},
		{"会话/流", "streams_total", strconv.FormatUint(r.StreamsTotal, 10)},

		{"载荷速率", "payload_sent", rateText(v, v.Sample.PayloadSentBps)},
		{"载荷速率", "payload_recv", rateText(v, v.Sample.PayloadRecvBps)},

		{"线速率", "wire_sent", rateText(v, v.Sample.WireSentBps)},
		{"线速率", "wire_recv", rateText(v, v.Sample.WireRecvBps)},

		{"累计量", "payload_sent_total", HumanBytes(float64(r.PayloadSent))},
		{"累计量", "payload_recv_total", HumanBytes(float64(r.PayloadReceived))},
		{"累计量", "wire_sent_total", HumanBytes(float64(v.Now.Transport.UDPBytesSent))},
		{"累计量", "wire_recv_total", HumanBytes(float64(v.Now.Transport.UDPBytesReceived))},

		{"链路质量", "retrans", retransValue(v)},
		{"链路质量", "lost_segs", deltaValue(v, v.Sample.LostSegsDelta)},
		{"链路质量", "repeat_segs", deltaValue(v, v.Sample.RepeatSegsDelta)},
		{"链路质量", "kcp_in_errors", deltaValue(v, v.Sample.KCPInErrorsDelta)},
		{"链路质量", "fec_recovered", deltaValue(v, v.Sample.FECRecoveredDelta)},
		{"链路质量", "fec_errs", deltaValue(v, v.Sample.FECErrsDelta)},
	}
	if v.HasPool {
		rows = append(rows,
			tableRow{"会话池", "pool_in_use", strconv.Itoa(v.Now.Pool.InUse)},
			tableRow{"会话池", "pool_idle", strconv.Itoa(v.Now.Pool.Idle)},
			tableRow{"会话池", "pool_creating", strconv.Itoa(v.Now.Pool.Creating)},
			tableRow{"会话池", "pool_waiters", strconv.Itoa(v.Now.Pool.Waiters)},
			tableRow{"会话池", "pool_rebuilds", strconv.FormatUint(v.Now.Pool.Rebuilds, 10)},
		)
	}
	rows = append(rows,
		tableRow{"错误", "auth", strconv.FormatUint(r.AuthFailures, 10)},
		tableRow{"错误", "session", strconv.FormatUint(r.SessionDialFailures, 10)},
		tableRow{"错误", "socks5", strconv.FormatUint(r.Socks5Failures, 10)},
		tableRow{"错误", "dial", strconv.FormatUint(r.DialFailures, 10)},
	)
	return rows
}

func rateText(v View, bps float64) string {
	if !v.HasRates {
		return "n/a"
	}
	return HumanRate(bps)
}

func deltaValue(v View, n uint64) string {
	if !v.HasRates {
		return "n/a"
	}
	return strconv.FormatUint(n, 10)
}

func retransValue(v View) string {
	if !v.HasRates {
		return "n/a"
	}
	return retransText(v.Sample)
}

func columnWidths(rows []tableRow) (int, int, int) {
	w1, w2, w3 := 0, 0, 0
	for _, r := range rows {
		w1 = max(w1, displayWidth(r.group))
		w2 = max(w2, displayWidth(r.name))
		w3 = max(w3, displayWidth(r.value))
	}
	return w1, w2, w3
}

// border 画一条横线：三条列宽分别为 w1/w2/w3，单元格左右各留一个空格。
func border(left, mid, right string, w1, w2, w3 int) string {
	seg := func(w int) string { return strings.Repeat("─", w+2) }
	return left + seg(w1) + mid + seg(w2) + mid + seg(w3) + right
}

// pad 按显示宽度右侧补空格（中文字符按 2 列计）。
func pad(s string, w int) string {
	if n := w - displayWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// FormatUptime 把运行时长格式化成 1h02m03s / 2m03s / 5s。
func FormatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Truncate(time.Second)
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	s := (d % time.Minute) / time.Second
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// displayWidth 返回字符串在等宽终端里占用的列数。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// runeWidth 是 wcwidth 的常用近似：CJK/全角字符占 2 列，控制字符占 0 列，其余 1 列。
func runeWidth(r rune) int {
	switch {
	case r < 0x20 || (r >= 0x7F && r < 0xA0):
		return 0
	case r >= 0x1100 && (r <= 0x115F ||
		r == 0x2329 || r == 0x232A ||
		(r >= 0x2E80 && r <= 0xA4CF && r != 0x303F) ||
		(r >= 0xAC00 && r <= 0xD7A3) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xFE30 && r <= 0xFE6F) ||
		(r >= 0xFF00 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6)):
		return 2
	default:
		return 1
	}
}
