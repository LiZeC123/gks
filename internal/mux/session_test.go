package mux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/protocol"
	"github.com/LiZeC123/gks/internal/transport"
)

var testPSK = []byte("0123456789abcdef0123456789abcdef")

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testTransportOptions() transport.Options {
	return transport.Options{
		Interval: 10 * time.Millisecond,
		MTU:      1350,
		SndWnd:   256,
		RcvWnd:   256,
		Crypt:    "none",
	}
}

func newReplayCache(t *testing.T) *protocol.ReplayCache {
	t.Helper()
	cache, err := protocol.NewReplayCache(64, 2*time.Minute)
	if err != nil {
		t.Fatalf("NewReplayCache: %v", err)
	}
	return cache
}

func serverOptions(cache *protocol.ReplayCache, psk []byte, heartbeat time.Duration) Options {
	return Options{
		Role:              RoleServer,
		PSK:               psk,
		AEAD:              protocol.AEADChaCha20Poly1305,
		AuthTimeout:       2 * time.Second,
		TimestampWindow:   time.Minute,
		ReplayCache:       cache,
		HeartbeatInterval: heartbeat,
		HeartbeatMiss:     3,
		Logger:            quietLogger(),
	}
}

func clientOptions(psk []byte, heartbeat time.Duration) Options {
	return Options{
		Role:              RoleClient,
		PSK:               psk,
		AEAD:              protocol.AEADChaCha20Poly1305,
		AuthTimeout:       2 * time.Second,
		HeartbeatInterval: heartbeat,
		HeartbeatMiss:     3,
		Logger:            quietLogger(),
	}
}

func mustListen(t *testing.T) *transport.Listener {
	t.Helper()
	ln, err := transport.Listen("127.0.0.1:0", testTransportOptions())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func TestSessionHandshakeEchoAndHeartbeat(t *testing.T) {
	ln := mustListen(t)
	srvOpts := serverOptions(newReplayCache(t), testPSK, 50*time.Millisecond)

	srvCh := make(chan *Session, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		s, err := AcceptSession(context.Background(), conn, srvOpts)
		if err != nil {
			errCh <- err
			return
		}
		s.Handle(protocol.TypeTestEcho, func(s *Session, f protocol.Frame) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = s.SendControl(ctx, protocol.TypeTestEcho, f.Payload)
		})
		srvCh <- s
	}()

	conn, err := transport.NewDialer(testTransportOptions()).Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	client, err := DialSession(context.Background(), conn, clientOptions(testPSK, 50*time.Millisecond))
	if err != nil {
		t.Fatalf("DialSession: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	var server *Session
	select {
	case server = <-srvCh:
		t.Cleanup(func() { _ = server.Close() })
	case err := <-errCh:
		t.Fatalf("服务端握手失败: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("等待服务端会话超时")
	}

	echoCh := make(chan []byte, 1)
	client.Handle(protocol.TypeTestEcho, func(s *Session, f protocol.Frame) {
		echoCh <- append([]byte(nil), f.Payload...)
	})

	payload := []byte("hello gks phase 2")
	if err := client.SendControl(context.Background(), protocol.TypeTestEcho, payload); err != nil {
		t.Fatalf("SendControl: %v", err)
	}
	select {
	case got := <-echoCh:
		if !bytes.Equal(got, payload) {
			t.Fatalf("echo = %q，期望 %q", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待 echo 超时")
	}

	if !client.Stats().Secure {
		t.Fatal("会话应处于安全态")
	}

	deadline := time.Now().Add(5 * time.Second)
	for client.PongsReceived() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("未收到 PONG（pings_sent=%d）", client.PingsSent())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if server.Stats().PingsSent != 0 {
		// 服务端也可发心跳，此处只要求客户端心跳生效。
		t.Logf("服务端也发了 %d 次 PING", server.Stats().PingsSent)
	}
}

func TestSessionRejectsWrongPSK(t *testing.T) {
	ln := mustListen(t)
	srvOpts := serverOptions(newReplayCache(t), testPSK, 0)
	wrongPSK := []byte("ffffffffffffffffffffffffffffffff")

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		s, err := AcceptSession(context.Background(), conn, srvOpts)
		if s != nil {
			_ = s.Close()
		}
		srvErr <- err
	}()

	conn, err := transport.NewDialer(testTransportOptions()).Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	client, err := DialSession(context.Background(), conn, clientOptions(wrongPSK, 0))
	if err == nil {
		_ = client.Close()
		t.Fatal("错误 PSK 不应握手成功")
	}
	if !errors.Is(err, protocol.ErrAuthFailed) {
		t.Fatalf("客户端 err = %v，期望 ErrAuthFailed", err)
	}

	select {
	case err := <-srvErr:
		if err == nil {
			t.Fatal("服务端不应握手成功")
		}
		if !errors.Is(err, protocol.ErrAuthFailed) {
			t.Fatalf("服务端 err = %v，期望 ErrAuthFailed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待服务端结果超时")
	}
}

// TestSessionRejectsPlaintextInSecureState 验证「不降级」：
// 安全态下收到明文帧必须断开，而不是退回明文解析。
func TestSessionRejectsPlaintextInSecureState(t *testing.T) {
	ln := mustListen(t)
	srvOpts := serverOptions(newReplayCache(t), testPSK, 0)

	srvCh := make(chan *Session, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		s, err := AcceptSession(context.Background(), conn, srvOpts)
		if err != nil {
			return
		}
		srvCh <- s
	}()

	conn, err := transport.NewDialer(testTransportOptions()).Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// 手工完成认证，但对端不发 AUTH_RESP 之外的任何东西。
	init, err := protocol.NewInitiator(protocol.HandshakeConfig{
		PSK:  testPSK,
		AEAD: protocol.AEADChaCha20Poly1305,
	})
	if err != nil {
		t.Fatalf("NewInitiator: %v", err)
	}
	req, err := init.NewRequest()
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	raw, err := protocol.MarshalPlain(protocol.Frame{Type: protocol.TypeAuthReq, Payload: req.Marshal()})
	if err != nil {
		t.Fatalf("MarshalPlain: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if _, err := conn.Write(raw); err != nil {
		t.Fatalf("写 AUTH_REQ: %v", err)
	}
	hdr, body, err := protocol.ReadFrame(conn, protocol.MaxFrameBody)
	if err != nil {
		t.Fatalf("读 AUTH_RESP: %v", err)
	}
	f, err := protocol.UnmarshalPlainBody(hdr, body)
	if err != nil {
		t.Fatalf("解析 AUTH_RESP: %v", err)
	}
	resp, err := protocol.ParseAuthResponse(f.Payload)
	if err != nil {
		t.Fatalf("ParseAuthResponse: %v", err)
	}
	if _, err := init.Complete(req, resp); err != nil {
		t.Fatalf("认证失败: %v", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("清除 deadline: %v", err)
	}

	var server *Session
	select {
	case server = <-srvCh:
	case <-time.After(5 * time.Second):
		t.Fatal("等待服务端会话超时")
	}
	t.Cleanup(func() { _ = server.Close() })

	plain, err := protocol.MarshalPlain(protocol.Frame{
		Type:     protocol.TypeData,
		StreamID: 1,
		Payload:  []byte("plaintext in secure state"),
	})
	if err != nil {
		t.Fatalf("MarshalPlain: %v", err)
	}
	if _, err := conn.Write(plain); err != nil {
		t.Fatalf("写明文帧: %v", err)
	}

	select {
	case <-server.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("服务端未因安全态收到明文帧而断开")
	}
	if err := server.Wait(); !errors.Is(err, protocol.ErrDecrypt) {
		t.Fatalf("关闭原因 = %v，期望 ErrDecrypt", err)
	}
}

// TestSessionHeartbeatTimeout 用 net.Pipe 模拟「完成认证后对端失联」。
func TestSessionHeartbeatTimeout(t *testing.T) {
	clientConn, peerConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = peerConn.Close()
	})

	peerReady := make(chan error, 1)
	go func() {
		defer func() { _ = peerConn.Close() }()
		auth, err := protocol.NewAuthenticator(protocol.AuthenticatorConfig{
			HandshakeConfig: protocol.HandshakeConfig{
				PSK:  testPSK,
				AEAD: protocol.AEADChaCha20Poly1305,
			},
			Window: time.Minute,
			Replay: newReplayCache(t),
		})
		if err != nil {
			peerReady <- err
			return
		}
		if err := peerConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			peerReady <- err
			return
		}
		hdr, body, err := protocol.ReadFrame(peerConn, protocol.MaxFrameBody)
		if err != nil {
			peerReady <- err
			return
		}
		f, err := protocol.UnmarshalPlainBody(hdr, body)
		if err != nil {
			peerReady <- err
			return
		}
		req, err := protocol.ParseAuthRequest(f.Payload)
		if err != nil {
			peerReady <- err
			return
		}
		resp, err := auth.Verify(req)
		if err != nil {
			peerReady <- err
			return
		}
		out, err := protocol.MarshalPlain(protocol.Frame{Type: protocol.TypeAuthResp, Payload: resp.Marshal()})
		if err != nil {
			peerReady <- err
			return
		}
		if _, err := peerConn.Write(out); err != nil {
			peerReady <- err
			return
		}
		_ = peerConn.SetDeadline(time.Time{})
		peerReady <- nil

		// 认证完成后只读不回：模拟对端失联。
		buf := make([]byte, 8192)
		for {
			if _, err := peerConn.Read(buf); err != nil {
				return
			}
		}
	}()

	opts := clientOptions(testPSK, 30*time.Millisecond)
	s, err := DialSession(context.Background(), clientConn, opts)
	if err != nil {
		t.Fatalf("DialSession: %v", err)
	}
	if err := <-peerReady; err != nil {
		t.Fatalf("对端脚本错误: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrHeartbeatTimeout) {
			t.Fatalf("关闭原因 = %v，期望 ErrHeartbeatTimeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("心跳超时未生效")
	}
	if got := s.PingsSent(); got < 3 {
		t.Fatalf("PING 次数 = %d，期望 >= 3", got)
	}
}

func TestSessionHandshakeTimeout(t *testing.T) {
	clientConn, peerConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = peerConn.Close()
	})

	srvOpts := serverOptions(newReplayCache(t), testPSK, 0)
	srvOpts.AuthTimeout = 100 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := AcceptSession(context.Background(), clientConn, srvOpts)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrAuthTimeout) {
			t.Fatalf("err = %v，期望 ErrAuthTimeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("认证超时未生效")
	}
}

func TestSessionOptionsValidation(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want error
	}{
		{"PSK 缺失", Options{}, ErrNoPSK},
		{"PSK 过短", Options{PSK: testPSK[:16]}, ErrNoPSK},
		{"AEAD 未知", Options{PSK: testPSK, AEAD: "rot13"}, protocol.ErrUnknownAEAD},
		{"Server 缺重放缓存", Options{Role: RoleServer, PSK: testPSK, TimestampWindow: time.Minute}, ErrNoReplayCache},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.opts.normalize(); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v，期望 %v", err, tc.want)
			}
		})
	}

	// 默认值填充。
	opts := Options{PSK: testPSK}
	if err := opts.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if opts.TokenID != protocol.DefaultTokenID || opts.AEAD != protocol.AEADChaCha20Poly1305 {
		t.Fatalf("默认值不正确: %+v", opts)
	}
	if opts.WriteQueue != 1024 || opts.HeartbeatMiss != 3 {
		t.Fatalf("默认值不正确: %+v", opts)
	}
}

func TestSendControlAfterClose(t *testing.T) {
	clientConn, peerConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = peerConn.Close()
	})

	go func() {
		auth, err := protocol.NewAuthenticator(protocol.AuthenticatorConfig{
			HandshakeConfig: protocol.HandshakeConfig{PSK: testPSK, AEAD: protocol.AEADChaCha20Poly1305},
			Window:          time.Minute,
			Replay:          newReplayCache(t),
		})
		if err != nil {
			return
		}
		hdr, body, err := protocol.ReadFrame(peerConn, protocol.MaxFrameBody)
		if err != nil {
			return
		}
		f, err := protocol.UnmarshalPlainBody(hdr, body)
		if err != nil {
			return
		}
		req, err := protocol.ParseAuthRequest(f.Payload)
		if err != nil {
			return
		}
		resp, err := auth.Verify(req)
		if err != nil {
			return
		}
		out, _ := protocol.MarshalPlain(protocol.Frame{Type: protocol.TypeAuthResp, Payload: resp.Marshal()})
		_, _ = peerConn.Write(out)
		buf := make([]byte, 4096)
		for {
			if _, err := peerConn.Read(buf); err != nil {
				return
			}
		}
	}()

	s, err := DialSession(context.Background(), clientConn, clientOptions(testPSK, 0))
	if err != nil {
		t.Fatalf("DialSession: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-s.Done()
	if err := s.SendControl(context.Background(), protocol.TypePing, nil); err == nil {
		t.Fatal("会话关闭后发送应失败")
	}
	// 重复关闭必须幂等。
	if err := s.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	if got := s.Conv(); got != 0 {
		t.Fatalf("net.Pipe 会话的 conv = %d，期望 0", got)
	}
	if s.Role() != RoleClient {
		t.Fatalf("角色 = %v", s.Role())
	}
}
