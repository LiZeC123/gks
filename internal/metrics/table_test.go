package metrics

import (
	"strings"
	"testing"
	"time"
)

func sampleView(t *testing.T) View {
	t.Helper()
	reg := &Registry{}
	reg.SessionsActive.Add(2)
	reg.StreamsActive.Add(1)
	reg.SessionsTotal.Add(12)
	reg.StreamsTotal.Add(418)
	reg.PayloadSent.Add(27 * 1024 * 1024)
	reg.PayloadReceived.Add(142 * 1024)
	reg.AuthFailures.Add(1)
	reg.DialFailures.Add(2)

	now := time.Unix(1700000000, 0)
	prev := reg.Take(TransportStats{UDPBytesSent: 50_000_000}, PoolStats{InUse: 1, Idle: 1}, now.Add(-10*time.Second))
	cur := reg.Take(TransportStats{
		UDPBytesSent:     50_000_000 + 27_000_000,
		UDPBytesReceived: 900_000,
		OutSegs:          200,
		RetransSegs:      20,
		LostSegs:         6,
		RepeatSegs:       3,
		KCPInErrors:      1,
		FECRecovered:     40,
		FECErrs:          2,
	}, PoolStats{InUse: 1, Idle: 1, Waiters: 2, Rebuilds: 7}, now)

	return View{
		Now:        cur,
		Sample:     computeSample(prev, cur),
		HasRates:   true,
		UserWindow: 10 * time.Second,
		Window:     cur.At.Sub(prev.At),
		HasPool:    true,
	}
}

func TestRenderTableLayout(t *testing.T) {
	v := sampleView(t)
	out := RenderTable(v, TableOptions{Role: "client", StartedAt: v.Now.At.Add(-time.Hour), Now: v.Now.At})

	if strings.Contains(out, "\x1b") {
		t.Fatalf("表格文本不应包含 ANSI 转义: %q", out)
	}
	for _, want := range []string{
		"gks client", "运行 1h00m00s", "窗口 10s", v.Now.At.Format("2006-01-02 15:04:05"),
		"会话/流", "载荷速率", "线速率", "累计量", "链路质量", "会话池", "错误",
		"sessions", "streams_total", "payload_sent", "wire_recv_total",
		"retrans", "fec_recovered", "pool_waiters", "socks5",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("表格缺少 %q:\n%s", want, out)
		}
	}
	// 重传率 20/200 = 10%。
	if !strings.Contains(out, "10.00%") {
		t.Fatalf("重传率渲染不对:\n%s", out)
	}
	// 告警行：pool_waiters=2、retrans 10%、fec_errs=2、kcp_in_errors=1。
	if !strings.Contains(out, "[告警] ") || !strings.Contains(out, "pool_waiters=2") {
		t.Fatalf("告警行不对:\n%s", out)
	}
	if !strings.Contains(out, "fec_errs=2") || !strings.Contains(out, "kcp_in_errors=1") {
		t.Fatalf("告警行缺少链路项:\n%s", out)
	}

	// 所有非空行的显示宽度必须一致（中文字符按 2 列计），否则终端里会歪。
	widths := map[int]bool{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "[告警]") || strings.HasPrefix(line, "gks ") {
			continue
		}
		widths[displayWidth(line)] = true
	}
	if len(widths) != 1 {
		t.Fatalf("表格各行宽度不一致: %v\n%s", widths, out)
	}
}

func TestRenderTableWithoutRatesOrPool(t *testing.T) {
	reg := &Registry{}
	reg.SessionsActive.Add(1)
	now := time.Unix(1700000000, 0)
	v := View{
		Now:        reg.Take(TransportStats{}, PoolStats{}, now),
		Sample:     Sample{Current: reg.Take(TransportStats{}, PoolStats{}, now)},
		HasRates:   false,
		UserWindow: 10 * time.Second,
		HasPool:    false,
	}
	out := RenderTable(v, TableOptions{Role: "server", StartedAt: now.Add(-90 * time.Second), Now: now})

	if !strings.Contains(out, "速率历史不足") {
		t.Fatalf("应提示历史不足:\n%s", out)
	}
	if !strings.Contains(out, "[告警] 无") {
		t.Fatalf("无告警时应有固定的告警行:\n%s", out)
	}
	if strings.Contains(out, "会话池") {
		t.Fatalf("服务端不应有会话池分组:\n%s", out)
	}
	if strings.Count(out, "n/a") < 6 {
		t.Fatalf("速率与增量列应为 n/a:\n%s", out)
	}
	if !strings.Contains(out, "运行 1m30s") {
		t.Fatalf("运行时长格式不对:\n%s", out)
	}
}

func TestAlarmsRules(t *testing.T) {
	if got := Alarms(View{}); len(got) != 0 {
		t.Fatalf("无数据时不应告警: %v", got)
	}
	v := View{
		HasRates: true,
		HasPool:  true,
		Sample:   Sample{OutSegsDelta: 10, RetransRate: 0.9},
	}
	if got := Alarms(v); len(got) != 0 {
		t.Fatalf("小样本重传不应告警: %v", got)
	}
	v.Sample.OutSegsDelta = 100
	if got := Alarms(v); len(got) != 1 {
		t.Fatalf("重传率 90%% 应告警: %v", got)
	}
	v.Sample.RetransRate = 0.005
	if got := Alarms(v); len(got) != 0 {
		t.Fatalf("重传率 0.5%% 不应告警: %v", got)
	}
}

func TestFormatUptime(t *testing.T) {
	cases := map[time.Duration]string{
		0:                            "0s",
		5 * time.Second:              "5s",
		90 * time.Second:             "1m30s",
		time.Hour + 2*time.Minute:    "1h02m00s",
		26*time.Hour + 3*time.Second: "26h00m03s",
		-3 * time.Second:             "0s",
	}
	for in, want := range cases {
		if got := FormatUptime(in); got != want {
			t.Fatalf("FormatUptime(%s) = %q，期望 %q", in, got, want)
		}
	}
}

func TestDisplayWidth(t *testing.T) {
	cases := map[string]int{
		"abc":   3,
		"会话/流":  7,
		"│ a │": 5,
		"─":     1,
	}
	for in, want := range cases {
		if got := displayWidth(in); got != want {
			t.Fatalf("displayWidth(%q) = %d，期望 %d", in, got, want)
		}
	}
}
