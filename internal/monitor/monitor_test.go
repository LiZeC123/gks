package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/metrics"
)

// syncBuffer 是并发安全的缓冲：控制台 goroutine 与测试读取会同时访问它。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testCollector() *metrics.Collector {
	return metrics.NewCollector(metrics.Options{
		Interval: time.Second,
		Sources: metrics.Sources{
			Transport: func() metrics.TransportStats {
				return metrics.TransportStats{UDPBytesSent: 4096, OutSegs: 10}
			},
			Pool: func() metrics.PoolStats { return metrics.PoolStats{InUse: 1, Idle: 1} },
		},
	})
}

func TestStartWithoutEndpointAndConsole(t *testing.T) {
	m, err := Start(context.Background(), Options{Collector: testCollector(), ConsoleOut: io.Discard})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m.Addr() != "" {
		t.Fatalf("未配置端点时不应监听: %s", m.Addr())
	}
	m.Close()
}

func TestStartServesMetricsAndHealthz(t *testing.T) {
	m, err := Start(context.Background(), Options{
		Role:       "client",
		Collector:  testCollector(),
		HTTPAddr:   "127.0.0.1:0",
		ConsoleOut: io.Discard,
		StartedAt:  time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()

	if m.Addr() == "" {
		t.Fatal("应报告实际监听地址")
	}
	base := "http://" + m.Addr()

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("解析 /metrics: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || payload["role"] != "client" {
		t.Fatalf("/metrics 响应不对: %d %v", resp.StatusCode, payload)
	}
	if pool := payload["pool"].(map[string]any); pool["available"] != true {
		t.Fatalf("客户端 pool.available 应为 true: %v", pool)
	}

	resp2, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("/healthz 状态码 = %d", resp2.StatusCode)
	}
}

func TestStartFailsFastWhenEndpointBusy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	defer func() { _ = ln.Close() }()

	m, err := Start(context.Background(), Options{
		Collector:  testCollector(),
		HTTPAddr:   ln.Addr().String(),
		ConsoleOut: io.Discard,
	})
	if err == nil {
		m.Close()
		t.Fatal("端口被占用时应当启动失败")
	}
	if !strings.Contains(err.Error(), "统计端点监听") {
		t.Fatalf("错误信息应说明端点监听失败: %v", err)
	}
}

func TestStartRequiresCollector(t *testing.T) {
	if _, err := Start(context.Background(), Options{}); err == nil {
		t.Fatal("缺少 Collector 应当报错")
	}
}

// TestConsoleAppendsPlainTableWhenNotTTY 验证重定向场景：每周期追加一份纯文本表格，
// 不含任何 ANSI 转义。
func TestConsoleAppendsPlainTableWhenNotTTY(t *testing.T) {
	var buf bytes.Buffer
	m := &Monitor{
		opts: Options{
			Role:      "server",
			Collector: testCollector(),
			Console:   true,
			StartedAt: time.Now(),
		},
		out: &buf,
		tty: false,
	}
	m.render()
	m.render()

	out := buf.String()
	if strings.Contains(out, "\x1b") {
		t.Fatalf("非 TTY 输出不应包含 ANSI 转义: %q", out)
	}
	if got := strings.Count(out, "gks server"); got != 2 {
		t.Fatalf("每次渲染应追加一份表格，实际 %d 份:\n%s", got, out)
	}
}

// TestDrawRefreshesInPlaceWhenTTY 验证 TTY 场景使用「光标上移 + 清行」原地刷新。
func TestDrawRefreshesInPlaceWhenTTY(t *testing.T) {
	var buf bytes.Buffer
	m := &Monitor{
		opts: Options{Role: "client", Collector: testCollector(), Console: true, StartedAt: time.Now()},
		out:  &buf,
		tty:  true,
	}
	m.render()
	first := buf.String()
	if strings.Contains(first, "\x1b[") {
		t.Fatalf("首次渲染不应有光标移动: %q", first)
	}
	lines := strings.Count(first, "\n")
	if lines == 0 {
		t.Fatal("表格应有内容")
	}

	buf.Reset()
	m.render()
	second := buf.String()
	if !strings.HasPrefix(second, "\x1b["+strconv.Itoa(lines)+"A") {
		t.Fatalf("第二次渲染应从表格首行开始重画: %q", second)
	}
	if !strings.Contains(second, "\x1b[2K") {
		t.Fatalf("重画时应清行: %q", second)
	}
	// 行高固定 → 窗口内总行数不变。
	if got := strings.Count(second, "\n"); got != lines {
		t.Fatalf("行数应保持稳定：%d != %d", got, lines)
	}
}

func TestCloseRendersFinalTable(t *testing.T) {
	var buf syncBuffer
	m, err := Start(context.Background(), Options{
		Role:       "client",
		Collector:  testCollector(),
		Console:    true,
		ConsoleOut: &buf,
		StartedAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 启动后应立刻渲染一次。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(buf.String(), "gks client") {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "gks client") {
		t.Fatal("启动后应立即渲染一次表格")
	}
	before := strings.Count(buf.String(), "gks client")
	m.Close()
	if got := strings.Count(buf.String(), "gks client"); got <= before {
		t.Fatalf("Close 应再渲染一次终态表格: %d -> %d", before, got)
	}
}

func TestIsTerminal(t *testing.T) {
	if IsTerminal(&bytes.Buffer{}) {
		t.Fatal("bytes.Buffer 不应被当成终端")
	}
	if IsTerminal(io.Discard) {
		t.Fatal("io.Discard 不应被当成终端")
	}
}
