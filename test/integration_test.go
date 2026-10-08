// Package integration 是端到端集成测试（dev.md §9.2）：
// 在同一进程内启动 gks 服务端与客户端，通过本地 SOCKS5 代理访问本地 HTTP 目标。
package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"github.com/LiZeC123/gks/internal/client"
	"github.com/LiZeC123/gks/internal/mux"
	"github.com/LiZeC123/gks/internal/protocol"
	"github.com/LiZeC123/gks/internal/server"
	"github.com/LiZeC123/gks/internal/transport"
)

var testPSK = []byte("0123456789abcdef0123456789abcdef")

// bigBodySize 用于验证跨多帧的大body 传输。
const bigBodySize = 512 * 1024

func testTransportOptions() transport.Options {
	return transport.Options{
		Interval: 10 * time.Millisecond,
		MTU:      1350,
		SndWnd:   256,
		RcvWnd:   256,
		Crypt:    "none",
	}
}

// tracker 管理动态产生的 goroutine，并保证 Add 不与 Wait 竞争。
type tracker struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	closed bool
}

func (tr *tracker) go_(fn func()) {
	tr.mu.Lock()
	if tr.closed {
		tr.mu.Unlock()
		return
	}
	tr.wg.Add(1)
	tr.mu.Unlock()
	go func() {
		defer tr.wg.Done()
		fn()
	}()
}

func (tr *tracker) wait() {
	tr.mu.Lock()
	tr.closed = true
	tr.mu.Unlock()
	tr.wg.Wait()
}

// harness 是一套跑在本地的 目标服务 + gks 服务端 + gks 客户端。
type harness struct {
	targetURL string
	socksAddr string
	shutdown  func()
}

func startHarness(t *testing.T) *harness {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	// 1) 目标 HTTP 服务
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.WriteHeader(http.StatusOK)
			chunk := bytes.Repeat([]byte{'x'}, 16*1024)
			for written := 0; written < bigBodySize; written += len(chunk) {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		default:
			fmt.Fprintf(w, "target-ok:%s:%s", r.URL.Path, r.Host)
		}
	}))
	t.Cleanup(target.Close)

	// 2) gks 服务端
	srvLn, err := transport.Listen("127.0.0.1:0", testTransportOptions())
	if err != nil {
		t.Fatalf("服务端监听失败: %v", err)
	}
	replay, err := protocol.NewReplayCache(4096, 2*time.Minute)
	if err != nil {
		t.Fatalf("重放缓存: %v", err)
	}
	srvHandler := server.NewHandler(server.HandlerConfig{
		SessionOptions: mux.Options{
			PSK:               testPSK,
			AEAD:              protocol.AEADChaCha20Poly1305,
			AuthTimeout:       3 * time.Second,
			TimestampWindow:   time.Minute,
			ReplayCache:       replay,
			StreamIdleTimeout: 5 * time.Second,
			Logger:            quiet,
		},
		DialTimeout: 5 * time.Second,
		Keepalive:   30 * time.Second,
	})

	// 3) gks 客户端 + 本地 SOCKS5 监听
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("SOCKS5 监听失败: %v", err)
	}
	cliHandler := client.NewHandler(client.HandlerConfig{
		ServerAddr: srvLn.Addr().String(),
		SessionOptions: mux.Options{
			PSK:               testPSK,
			AEAD:              protocol.AEADChaCha20Poly1305,
			AuthTimeout:       3 * time.Second,
			HeartbeatInterval: time.Second,
			HeartbeatMiss:     3,
			StreamIdleTimeout: 5 * time.Second,
			Logger:            quiet,
		},
		HandshakeTimeout: 5 * time.Second,
		ConnectTimeout:   5 * time.Second,
	}, transport.NewDialer(testTransportOptions()))

	ctx, cancel := context.WithCancel(context.Background())
	tr := &tracker{}
	tr.go_(func() { acceptLoop(srvLn, func(c net.Conn) { srvHandler.Handle(ctx, c) }, tr) })
	tr.go_(func() { acceptLoop(socksLn, func(c net.Conn) { cliHandler.Handle(ctx, c) }, tr) })

	h := &harness{
		targetURL: target.URL,
		socksAddr: socksLn.Addr().String(),
		shutdown: func() {
			cancel()
			_ = srvLn.Close()
			_ = socksLn.Close()
			tr.wait()
		},
	}
	t.Cleanup(h.shutdown)
	return h
}

func acceptLoop(ln net.Listener, handle func(net.Conn), tr *tracker) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		tr.go_(func() { handle(conn) })
	}
}

// httpClient 返回一个走 SOCKS5 代理的 HTTP 客户端（使用 golang.org/x/net/proxy，
// 顺带验证与标准库生态的兼容性，见 dev.md §9.6）。
func (h *harness) httpClient(t *testing.T) *http.Client {
	t.Helper()
	d, err := proxy.SOCKS5("tcp", h.socksAddr, nil, proxy.Direct)
	if err != nil {
		t.Fatalf("构造 SOCKS5 dialer: %v", err)
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		t.Fatal("SOCKS5 dialer 未实现 ContextDialer")
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:       cd.DialContext,
			DisableKeepAlives: true,
		},
		Timeout: 15 * time.Second,
	}
}

func (h *harness) get(t *testing.T, rawURL string) string {
	t.Helper()
	client := h.httpClient(t)
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	return string(body)
}

func TestHTTPThroughProxyIPv4Target(t *testing.T) {
	h := startHarness(t)

	// httptest 的 URL 是 127.0.0.1 形式 → 走 ATYP=IPv4 路径。
	got := h.get(t, h.targetURL+"/hello")
	if !strings.Contains(got, "target-ok:/hello") {
		t.Fatalf("响应体 = %q", got)
	}
}

func TestHTTPThroughProxyDomainTarget(t *testing.T) {
	h := startHarness(t)

	// 把主机换成本地域名 → 走 ATYP=domain，由服务端侧解析（dev.md §0.2 远程 DNS）。
	_, port, err := net.SplitHostPort(strings.TrimPrefix(h.targetURL, "http://"))
	if err != nil {
		t.Fatalf("拆分目标地址: %v", err)
	}
	got := h.get(t, "http://localhost:"+port+"/domain")
	if !strings.Contains(got, "target-ok:/domain") {
		t.Fatalf("响应体 = %q", got)
	}
}

func TestProxyLargeBody(t *testing.T) {
	h := startHarness(t)
	got := h.get(t, h.targetURL+"/big")
	if len(got) != bigBodySize {
		t.Fatalf("大body 长度 = %d，期望 %d", len(got), bigBodySize)
	}
}

func TestProxyConcurrentRequests(t *testing.T) {
	h := startHarness(t)
	client := h.httpClient(t)

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/c%d", i)
			resp, err := client.Get(h.targetURL + path)
			if err != nil {
				errs <- fmt.Errorf("请求 %s: %w", path, err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				errs <- fmt.Errorf("读取 %s: %w", path, err)
				return
			}
			if !strings.Contains(string(body), "target-ok:"+path) {
				errs <- fmt.Errorf("%s 响应体异常: %q", path, body)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestProxyConnectionRefusedCode(t *testing.T) {
	h := startHarness(t)

	// 先占一个端口再释放，得到一个几乎必然无人监听的地址。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	closedAddr := ln.Addr().String()
	_ = ln.Close()

	host, portStr, err := net.SplitHostPort(closedAddr)
	if err != nil {
		t.Fatalf("拆分地址: %v", err)
	}
	var port uint16
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("解析端口: %v", err)
	}
	target, err := protocol.AddressFromHostPort(host, port)
	if err != nil {
		t.Fatalf("构造目标: %v", err)
	}

	if rep := rawConnectReply(t, h.socksAddr, target); rep != protocol.RepConnectionRefused {
		t.Fatalf("REP = %s，期望 CONNECTION_REFUSED", protocol.RepString(rep))
	}
}

func TestProxyDNSCode(t *testing.T) {
	h := startHarness(t)

	target := protocol.Address{ATYP: protocol.ATYPDomain, Host: "no-such-host-xyz.invalid", Port: 80}
	if rep := rawConnectReply(t, h.socksAddr, target); rep != protocol.RepHostUnreachable {
		t.Fatalf("REP = %s，期望 HOST_UNREACHABLE", protocol.RepString(rep))
	}
}

// rawConnectReply 手工完成 SOCKS5 协商与 CONNECT，返回服务端回复的 REP 码。
func rawConnectReply(t *testing.T, socksAddr string, target protocol.Address) byte {
	t.Helper()
	conn, err := net.DialTimeout("tcp", socksAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("连接 SOCKS5: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("设置超时: %v", err)
	}

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("写方法协商: %v", err)
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil {
		t.Fatalf("读方法选择: %v", err)
	}
	if method[0] != 0x05 || method[1] != 0x00 {
		t.Fatalf("方法选择 = % x", method)
	}

	body, err := protocol.ConnectRequest{Address: target}.Marshal()
	if err != nil {
		t.Fatalf("编码目标: %v", err)
	}
	req := append([]byte{0x05, 0x01, 0x00}, body...)
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("写 CONNECT: %v", err)
	}
	var head [3]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		t.Fatalf("读回复: %v", err)
	}
	if head[0] != 0x05 {
		t.Fatalf("回复版本 = 0x%02X", head[0])
	}
	return head[1]
}
