package metrics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestHandler(t *testing.T, c *Collector, role string) http.Handler {
	t.Helper()
	now := time.Unix(1700000000, 0)
	return NewHTTPHandler(c, HTTPOptions{
		Role:          role,
		StartedAt:     now.Add(-time.Minute),
		DefaultWindow: 10 * time.Second,
		Now:           func() time.Time { return now },
	})
}

func TestHTTPMetricsWithoutRates(t *testing.T) {
	reg := &Registry{}
	reg.SessionsActive.Add(2)
	reg.StreamsActive.Add(1)
	reg.SessionsTotal.Add(12)
	reg.PayloadSent.Add(4096)
	reg.DialFailures.Add(3)

	c := NewCollector(Options{Registry: reg, Interval: 10 * time.Second})
	srv := httptest.NewServer(newTestHandler(t, c, "server"))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}

	var p map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("解析 JSON: %v", err)
	}
	if p["schema_version"].(float64) != 1 {
		t.Fatalf("schema_version = %v", p["schema_version"])
	}
	if p["role"] != "server" {
		t.Fatalf("role = %v", p["role"])
	}
	if p["rate_available"] != false {
		t.Fatalf("没有历史时 rate_available 应为 false: %v", p["rate_available"])
	}
	if got := p["rate_window_seconds"].(float64); got != 10 {
		t.Fatalf("rate_window_seconds = %v", got)
	}
	sessions := p["sessions"].(map[string]any)
	if sessions["active"].(float64) != 2 || sessions["total"].(float64) != 12 {
		t.Fatalf("sessions = %v", sessions)
	}
	// 速率字段应为 null（Go 里解成 nil）。
	payload := p["payload"].(map[string]any)
	if payload["sent_bps"] != nil || payload["recv_bps"] != nil {
		t.Fatalf("无速率时 sent_bps/recv_bps 应为 null: %v", payload)
	}
	if payload["sent_total"].(float64) != 4096 {
		t.Fatalf("payload.sent_total = %v", payload["sent_total"])
	}
	pool := p["pool"].(map[string]any)
	if pool["available"] != false {
		t.Fatalf("服务端 pool.available 应为 false: %v", pool)
	}
	link := p["link"].(map[string]any)
	win := link["window"].(map[string]any)
	if win["out_segs"] != nil || win["lost_segs"] != nil {
		t.Fatalf("无速率时窗口增量应为 null: %v", win)
	}
	errors := p["errors"].(map[string]any)
	if errors["dial"].(float64) != 3 {
		t.Fatalf("errors = %v", errors)
	}
	if alarms, ok := p["alarms"].([]any); !ok || len(alarms) != 0 {
		t.Fatalf("alarms 应为空数组: %v", p["alarms"])
	}
}

func TestHTTPMetricsWithRates(t *testing.T) {
	reg := &Registry{}
	cur := TransportStats{UDPBytesSent: 10_000, OutSegs: 100, RetransSegs: 20, FECErrs: 1}
	pool := PoolStats{InUse: 1, Waiters: 2, Rebuilds: 7}
	c := NewCollector(Options{
		Registry: reg,
		Interval: 10 * time.Second,
		Sources: Sources{
			Transport: func() TransportStats { return cur },
			Pool:      func() PoolStats { return pool },
		},
	})
	base := time.Now().Add(-30 * time.Second)
	c.Record(reg.Take(TransportStats{}, PoolStats{}, base))

	v := c.View(10 * time.Second)
	if !v.HasRates {
		t.Fatal("测试前提：应有速率")
	}
	p := buildMetricsPayload(v, HTTPOptions{Role: "client", StartedAt: v.Now.At}, v.Now.At)

	if !p.RateAvailable {
		t.Fatal("rate_available 应为 true")
	}
	if p.Wire.SentBps == nil || *p.Wire.SentBps <= 0 {
		t.Fatalf("wire.sent_bps = %v", p.Wire.SentBps)
	}
	if p.Payload.SentBps == nil {
		t.Fatal("payload.sent_bps 不应为 null")
	}
	if !p.Link.Window.RetransRatioAvailable || p.Link.Window.RetransRatio == nil {
		t.Fatalf("重传率应可用: %+v", p.Link.Window)
	}
	if got := *p.Link.Window.RetransRatio; got != 0.2 {
		t.Fatalf("重传率 = %v，期望 0.2", got)
	}
	if p.Link.Window.OutSegs == nil || *p.Link.Window.OutSegs != 100 {
		t.Fatalf("窗口发送段 = %v", p.Link.Window.OutSegs)
	}
	if p.Pool.Waiters != 2 || p.Pool.Rebuilds != 7 || !p.Pool.Available {
		t.Fatalf("池指标不对: %+v", p.Pool)
	}
	// 告警项：pool_waiters + 重传率 ≥1% + fec_errs。
	if len(p.Alarms) != 3 {
		t.Fatalf("告警项 = %v", p.Alarms)
	}
}

func TestHTTPWindowParameter(t *testing.T) {
	c := NewCollector(Options{Interval: 10 * time.Second})
	srv := httptest.NewServer(newTestHandler(t, c, "client"))
	defer srv.Close()

	cases := []struct {
		query  string
		status int
	}{
		{"", http.StatusOK},
		{"?window=1s", http.StatusOK},
		{"?window=10m", http.StatusOK},
		{"?window=500ms", http.StatusBadRequest},
		{"?window=11m", http.StatusBadRequest},
		{"?window=abc", http.StatusBadRequest},
		{"?window=10", http.StatusBadRequest},
	}
	for _, tc := range cases {
		resp, err := http.Get(srv.URL + "/metrics" + tc.query)
		if err != nil {
			t.Fatalf("GET /metrics%s: %v", tc.query, err)
		}
		if resp.StatusCode != tc.status {
			t.Fatalf("GET /metrics%s 状态码 = %d，期望 %d", tc.query, resp.StatusCode, tc.status)
		}
		_ = resp.Body.Close()
	}
}

func TestHTTPHealthz(t *testing.T) {
	c := NewCollector(Options{Interval: 10 * time.Second})
	srv := httptest.NewServer(newTestHandler(t, c, "client"))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var p map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("解析 JSON: %v", err)
	}
	if p["status"] != "ok" || p["role"] != "client" {
		t.Fatalf("healthz 内容 = %v", p)
	}
	if got := p["uptime_seconds"].(float64); got != 60 {
		t.Fatalf("uptime_seconds = %v", got)
	}
}

func TestHTTPNotFoundAndMethodNotAllowed(t *testing.T) {
	c := NewCollector(Options{Interval: 10 * time.Second})
	srv := httptest.NewServer(newTestHandler(t, c, "client"))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知路径状态码 = %d", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/metrics", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /metrics: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST 状态码 = %d", resp2.StatusCode)
	}
	if allow := resp2.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("Allow 头 = %q", allow)
	}

	// HEAD 应当被接受（net/http 会丢弃正文）。
	resp3, err := http.Head(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("HEAD /metrics: %v", err)
	}
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("HEAD 状态码 = %d", resp3.StatusCode)
	}
}
