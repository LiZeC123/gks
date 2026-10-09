package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/metrics"
)

func newTestState(t *testing.T, samples int) (*State, time.Time) {
	t.Helper()
	base := time.Unix(1700000000, 0)
	st := NewState(300)
	for i := 0; i < samples; i++ {
		i := i
		addSample(t, st, base.Add(time.Duration(i)*time.Second), func(p *metrics.Payload) {
			p.Streams.Total = 418 + uint64(i)*10
		})
	}
	return st, base
}

func newTestServer(t *testing.T, st *State, now time.Time, refresh time.Duration) *httptest.Server {
	t.Helper()
	srv, err := NewServer(ServerOptions{
		State: st,
		Page: PageOptions{
			PullURL:  "http://127.0.0.1:12081/metrics",
			Listen:   "0.0.0.0:12080",
			Interval: time.Second,
			Refresh:  refresh,
			Capacity: 300,
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func getBody(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应: %v", err)
	}
	return resp.StatusCode, string(raw), resp.Header
}

func TestServerPageRendersEverything(t *testing.T) {
	st, base := newTestState(t, 3)
	ts := newTestServer(t, st, base.Add(3*time.Second), 2*time.Second)

	status, body, header := getBody(t, ts.URL+"/")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	if ct := header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}
	for _, want := range []string{
		"<!doctype html>", "gks 监控面板", "上游可达", "client",
		"累计数据", "载荷上行累计", "27.0 MB", "窗口速率", "派生指标", "链路开销比",
		"趋势", `<svg class="chart"`, "链路明细", "会话池", "错误累计",
		`data-refresh-ms="2000"`, "/static/app.js",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("页面缺少 %q", want)
		}
	}
	// 上游 alarms 应转发到页面。
	if !strings.Contains(body, "pool_waiters=1") {
		t.Fatal("上游告警未渲染")
	}
	// 四张图都应有 SVG。
	if n := strings.Count(body, `<svg class="chart"`); n != 4 {
		t.Fatalf("SVG 数量 = %d，期望 4", n)
	}
}

func TestServerPartialIsFragment(t *testing.T) {
	st, base := newTestState(t, 2)
	ts := newTestServer(t, st, base.Add(2*time.Second), 2*time.Second)

	status, body, _ := getBody(t, ts.URL+"/partial")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	if strings.Contains(body, "<!doctype html>") || strings.Contains(body, "<html") {
		t.Fatalf("局部刷新应只返回内容片段:\n%s", body)
	}
	if !strings.Contains(body, "累计数据") || !strings.Contains(body, `<svg class="chart"`) {
		t.Fatalf("片段内容不完整:\n%s", body)
	}
}

func TestServerStaticAssets(t *testing.T) {
	st, base := newTestState(t, 1)
	ts := newTestServer(t, st, base, time.Second)

	status, css, header := getBody(t, ts.URL+"/static/style.css")
	if status != http.StatusOK || !strings.Contains(css, "--bg") {
		t.Fatalf("style.css 未内嵌: %d", status)
	}
	if ct := header.Get("Content-Type"); !strings.Contains(ct, "css") {
		t.Fatalf("css Content-Type = %q", ct)
	}
	status, js, _ := getBody(t, ts.URL+"/static/app.js")
	if status != http.StatusOK || !strings.Contains(js, "/partial") {
		t.Fatalf("app.js 未内嵌或内容不对: %d", status)
	}
	// 内嵌资源里不能出现外部引用，否则单文件分发会缺资源。
	for _, asset := range []string{css, js} {
		for _, bad := range []string{"http://", "https://", "//cdn"} {
			if strings.Contains(asset, bad) {
				t.Fatalf("内嵌资源引用了外部地址 %q", bad)
			}
		}
	}
}

func TestServerAPIState(t *testing.T) {
	st, base := newTestState(t, 3)
	now := base.Add(3 * time.Second)
	ts := newTestServer(t, st, now, 2*time.Second)

	status, body, header := getBody(t, ts.URL+"/api/state")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	if ct := header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var dto struct {
		SchemaVersion int `json:"schema_version"`
		Dashboard     struct {
			PullURL         string  `json:"pull_url"`
			Listen          string  `json:"listen"`
			IntervalSeconds float64 `json:"interval_seconds"`
			Samples         int     `json:"samples"`
			SampleCapacity  int     `json:"sample_capacity"`
		} `json:"dashboard"`
		Upstream struct {
			Reachable    bool     `json:"reachable"`
			Role         string   `json:"role"`
			StaleSeconds *float64 `json:"stale_seconds"`
		} `json:"upstream"`
		Latest *struct {
			SchemaVersion int             `json:"schema_version"`
			TotalErrors   uint64          `json:"total_errors"`
			ErrorsDelta   uint64          `json:"errors_delta"`
			StreamsDelta  uint64          `json:"streams_delta"`
			Payload       metrics.IORates `json:"payload"`
		} `json:"latest"`
		Derived struct {
			WirePayloadRatio *float64 `json:"wire_payload_ratio"`
		} `json:"derived"`
		Alarms  []string `json:"alarms"`
		History []struct {
			At             time.Time `json:"at"`
			PayloadSentBps *float64  `json:"payload_sent_bps"`
			RetransRatio   *float64  `json:"retrans_ratio"`
		} `json:"history"`
	}
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatalf("解析 /api/state: %v\n%s", err, body)
	}
	if dto.SchemaVersion != stateSchemaVersion {
		t.Fatalf("schema_version = %d", dto.SchemaVersion)
	}
	if dto.Dashboard.PullURL != "http://127.0.0.1:12081/metrics" || dto.Dashboard.Samples != 3 {
		t.Fatalf("dashboard 段不对: %+v", dto.Dashboard)
	}
	if dto.Dashboard.SampleCapacity != 300 || dto.Dashboard.IntervalSeconds != 1 {
		t.Fatalf("dashboard 参数不对: %+v", dto.Dashboard)
	}
	if !dto.Upstream.Reachable || dto.Upstream.Role != "client" {
		t.Fatalf("upstream 段不对: %+v", dto.Upstream)
	}
	if dto.Latest == nil || dto.Latest.TotalErrors != 3 {
		t.Fatalf("latest 段不对: %+v", dto.Latest)
	}
	// 第 3 条样本相对第 2 条的窗口增量：流 +10，错误 +0。
	if dto.Latest.StreamsDelta != 10 || dto.Latest.ErrorsDelta != 0 {
		t.Fatalf("窗口增量不对: streams=%d errors=%d", dto.Latest.StreamsDelta, dto.Latest.ErrorsDelta)
	}
	if dto.Derived.WirePayloadRatio == nil {
		t.Fatal("派生指标缺失")
	}
	if len(dto.History) != 3 {
		t.Fatalf("history 长度 = %d", len(dto.History))
	}
	if dto.History[0].PayloadSentBps == nil || dto.History[0].RetransRatio == nil {
		t.Fatalf("history 速率字段缺失: %+v", dto.History[0])
	}
	if len(dto.Alarms) == 0 {
		t.Fatal("alarms 应包含面板与上游告警")
	}
}

func TestServerHealthz(t *testing.T) {
	st, base := newTestState(t, 1)
	ts := newTestServer(t, st, base.Add(time.Second), time.Second)

	status, body, _ := getBody(t, ts.URL+"/healthz")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	var hz struct {
		Status       string   `json:"status"`
		Upstream     bool     `json:"upstream"`
		Role         string   `json:"role"`
		StaleSeconds *float64 `json:"stale_seconds"`
	}
	if err := json.Unmarshal([]byte(body), &hz); err != nil {
		t.Fatalf("解析 /healthz: %v", err)
	}
	if hz.Status != "ok" || !hz.Upstream || hz.Role != "client" {
		t.Fatalf("healthz = %+v", hz)
	}
	if hz.StaleSeconds == nil || *hz.StaleSeconds != 1 {
		t.Fatalf("stale_seconds = %v", hz.StaleSeconds)
	}

	// 上游不可达时面板自己仍然是 200（存活与上游健康是两件事）。
	st.MarkFailure(base.Add(2*time.Second), "connection refused")
	status, body, _ = getBody(t, ts.URL+"/healthz")
	if status != http.StatusOK {
		t.Fatalf("上游不可达时面板应仍返回 200，实际 %d", status)
	}
	if err := json.Unmarshal([]byte(body), &hz); err != nil {
		t.Fatalf("解析 /healthz: %v", err)
	}
	if hz.Upstream {
		t.Fatal("上游应标记为不可达")
	}
}

func TestServerWithoutData(t *testing.T) {
	st := NewState(300)
	st.MarkFailure(time.Unix(1700000000, 0), "dial tcp: connection refused")
	ts := newTestServer(t, st, time.Unix(1700000000, 1), time.Second)

	status, body, _ := getBody(t, ts.URL+"/")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	for _, want := range []string{"上游不可达", "尚未从上游取到数据", "connection refused"} {
		if !strings.Contains(body, want) {
			t.Fatalf("无数据页面缺少 %q", want)
		}
	}
	if strings.Contains(body, `<svg class="chart"`) {
		t.Fatal("无数据时不应渲染折线图")
	}

	status, body, _ = getBody(t, ts.URL+"/api/state")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	var dto struct {
		Latest  *json.RawMessage  `json:"latest"`
		History []json.RawMessage `json:"history"`
	}
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatalf("解析 /api/state: %v", err)
	}
	if dto.Latest != nil {
		t.Fatal("无数据时 latest 应为 null")
	}
	if len(dto.History) != 0 {
		t.Fatalf("无数据时 history 应为空数组，实际 %d", len(dto.History))
	}
	// 数组字段不能是 null，与 gks /metrics 的约定一致。
	if strings.Contains(body, `"alarms":null`) || strings.Contains(body, `"history":null`) {
		t.Fatalf("alarms/history 不应为 null: %s", body)
	}
	if !strings.Contains(body, `"history":[]`) {
		t.Fatalf("无样本时 history 应为 []: %s", body)
	}
}

func TestServerRefreshDisabled(t *testing.T) {
	st, base := newTestState(t, 1)
	ts := newTestServer(t, st, base, 0)

	_, body, _ := getBody(t, ts.URL+"/")
	if strings.Contains(body, "/static/app.js") {
		t.Fatal("refresh=0 时不应注入自动刷新脚本")
	}
}

func TestServerErrorsAndMethods(t *testing.T) {
	st, base := newTestState(t, 1)
	ts := newTestServer(t, st, base, time.Second)

	status, body, _ := getBody(t, ts.URL+"/nope")
	if status != http.StatusNotFound || !strings.Contains(body, "未知路径") {
		t.Fatalf("未知路径应 404: %d %s", status, body)
	}

	resp, err := http.Post(ts.URL+"/", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST 状态码 = %d", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("Allow = %q", allow)
	}

	// HEAD 应当被接受。
	resp, err = http.Head(ts.URL + "/api/state")
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD 状态码 = %d", resp.StatusCode)
	}
}

func TestNewServerRequiresState(t *testing.T) {
	if _, err := NewServer(ServerOptions{}); err == nil {
		t.Fatal("缺少 State 应当报错")
	}
}

func TestServerAPIStateEmptyAlarms(t *testing.T) {
	// 上游没有告警、面板自己也没有异常时，alarms 必须是 []（不是 null）。
	now := time.Unix(1700000000, 0)
	st := NewState(10)
	p := samplePayload(now)
	p.Alarms = nil
	p.Link.Window.RetransRatioAvailable = false
	p.Link.Window.RetransRatio = nil
	p.Pool.Waiters = 0
	st.MarkSuccess(Sample{At: now, Received: now, Metrics: p}, "")
	ts := newTestServer(t, st, now, time.Second)

	status, body, _ := getBody(t, ts.URL+"/api/state")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	if !strings.Contains(body, `"alarms":[]`) {
		t.Fatalf("无告警时应输出空数组: %s", body)
	}
}
