package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/metrics"
)

func TestNormalizeMetricsURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"127.0.0.1:12081", "http://127.0.0.1:12081/metrics", false},
		{" 127.0.0.1:12081 ", "http://127.0.0.1:12081/metrics", false},
		{"http://127.0.0.1:12081", "http://127.0.0.1:12081/metrics", false},
		{"http://127.0.0.1:12081/", "http://127.0.0.1:12081/metrics", false},
		{"https://example.com:443/custom", "https://example.com:443/custom", false},
		{"http://127.0.0.1:12081/metrics?window=5s", "http://127.0.0.1:12081/metrics?window=5s", false},
		{"", "", true},
		{"127.0.0.1", "", true},             // 缺端口
		{"ftp://127.0.0.1:12081", "", true}, // 协议不支持
		{"http://:12081", "", true},         // 缺主机
	}
	for _, tc := range cases {
		got, err := NormalizeMetricsURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("NormalizeMetricsURL(%q) 应当报错，实际 %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("NormalizeMetricsURL(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeMetricsURL(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestPollerFetchesAndStores(t *testing.T) {
	now := time.Unix(1700000000, 0)
	var gotWindow, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotWindow = r.URL.Query().Get("window")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(samplePayload(now))
	}))
	defer upstream.Close()

	url, err := NormalizeMetricsURL(upstream.URL)
	if err != nil {
		t.Fatalf("归一化拉取地址: %v", err)
	}
	st := NewState(10)
	p, err := NewPoller(PollerOptions{
		URL:      url,
		Interval: time.Second,
		Window:   2500 * time.Millisecond,
		Timeout:  time.Second,
		State:    st,
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if p.Window() != 2500*time.Millisecond {
		t.Fatalf("窗口 = %s", p.Window())
	}
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if gotPath != "/metrics" {
		t.Fatalf("请求路径 = %q", gotPath)
	}
	if gotWindow != "2.5s" {
		t.Fatalf("?window= %q，期望 2.5s", gotWindow)
	}
	snap := st.Snapshot()
	if !snap.Reachable || !snap.HasData() || len(snap.Samples) != 1 {
		t.Fatalf("状态不对: %+v", snap)
	}
	if snap.Samples[0].Metrics.Payload.SentBps == nil || *snap.Samples[0].Metrics.Payload.SentBps != 446545.9 {
		t.Fatalf("未解析出速率字段: %+v", snap.Samples[0].Metrics.Payload)
	}
}

func TestPollerWindowDefaultsToInterval(t *testing.T) {
	st := NewState(4)
	p, err := NewPoller(PollerOptions{
		URL: "http://127.0.0.1:1/metrics", Interval: 3 * time.Second, Timeout: time.Second,
		State: st, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if p.Window() != 3*time.Second {
		t.Fatalf("未指定 window 时应等于 interval，实际 %s", p.Window())
	}
}

func TestPollerToleratesUpstreamProblems(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"HTTP 500", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}},
		{"非法 JSON", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{not json"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(tc.handler)
			defer upstream.Close()
			st := NewState(4)
			p, err := NewPoller(PollerOptions{
				URL: upstream.URL + "/metrics", Interval: time.Second,
				Timeout: time.Second, State: st, Logger: discardLogger(),
			})
			if err != nil {
				t.Fatalf("NewPoller: %v", err)
			}
			if err := p.PollOnce(context.Background()); err == nil {
				t.Fatal("应当返回错误")
			}
			snap := st.Snapshot()
			if snap.Reachable {
				t.Fatal("失败后不应标记可达")
			}
			if snap.LastError == "" {
				t.Fatal("应记录错误信息")
			}
		})
	}
}

func TestPollerConnectionRefusedThenRecovers(t *testing.T) {
	// 先起一个上游拿到地址，再关掉它模拟「gks 没起来」。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(samplePayload(time.Now()))
	}))
	url := upstream.URL + "/metrics"
	upstream.Close()

	st := NewState(4)
	p, err := NewPoller(PollerOptions{URL: url, Interval: time.Second, Timeout: time.Second, State: st, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if err := p.PollOnce(context.Background()); err == nil {
		t.Fatal("上游不可达时应返回错误")
	}
	if st.Snapshot().Reachable {
		t.Fatal("不可达时不应标记可达")
	}

	// 换成可用的上游后应恢复。
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(samplePayload(time.Now()))
	}))
	defer upstream2.Close()
	p2, err := NewPoller(PollerOptions{URL: upstream2.URL + "/metrics", Interval: time.Second, Timeout: time.Second, State: st, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if err := p2.PollOnce(context.Background()); err != nil {
		t.Fatalf("恢复后 PollOnce: %v", err)
	}
	if !st.Snapshot().Reachable {
		t.Fatal("恢复后应标记可达")
	}
}

func TestPollerWarnsOnSchemaMismatchButKeepsData(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := samplePayload(time.Now())
		p.SchemaVersion = metrics.SchemaVersion + 1
		_ = json.NewEncoder(w).Encode(p)
	}))
	defer upstream.Close()

	st := NewState(4)
	p, err := NewPoller(PollerOptions{
		URL: upstream.URL + "/metrics", Interval: time.Second,
		Timeout: time.Second, State: st, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatalf("schema 不匹配不应算拉取失败: %v", err)
	}
	snap := st.Snapshot()
	if snap.SchemaWarn == "" {
		t.Fatal("应给出 schema 警告")
	}
	if !snap.HasData() {
		t.Fatal("schema 不匹配时仍应保留数据")
	}
}

func TestPollerRunStopsWithContext(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(samplePayload(time.Now()))
	}))
	defer upstream.Close()

	st := NewState(4)
	p, err := NewPoller(PollerOptions{
		URL: upstream.URL + "/metrics", Interval: 10 * time.Millisecond,
		Timeout: time.Second, State: st, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(st.Snapshot().Samples) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后退出")
	}
	if got := len(st.Snapshot().Samples); got < 2 {
		t.Fatalf("Run 应周期拉取，实际只有 %d 条样本", got)
	}
}

func TestNewPollerValidation(t *testing.T) {
	st := NewState(4)
	cases := []PollerOptions{
		{Interval: time.Second, Timeout: time.Second},                       // 缺 URL
		{URL: "http://x:1/m", Timeout: time.Second},                         // 缺 Interval
		{URL: "http://x:1/m", Interval: -time.Second, Timeout: time.Second}, // 负间隔
		{URL: "http://x:1/m", Interval: time.Second},                        // 缺 Timeout
		{URL: "http://x:1/m", Interval: time.Second, Timeout: time.Second},  // 缺 State
	}
	for i, o := range cases {
		o := o
		if i != 4 {
			o.State = st
		}
		if _, err := NewPoller(o); err == nil {
			t.Fatalf("第 %d 个用例应当报错: %+v", i, o)
		}
	}
}
