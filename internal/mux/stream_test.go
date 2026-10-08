package mux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/protocol"
)

// newSessionPair 建立一对通过 net.Pipe 相连、已完成认证的会话（默认关闭心跳）。
func newSessionPair(t *testing.T, mutate func(*Options)) (*Session, *Session) {
	t.Helper()
	cliConn, srvConn := net.Pipe()

	srvOpts := serverOptions(newReplayCache(t), testPSK, 0)
	cliOpts := clientOptions(testPSK, 0)
	if mutate != nil {
		mutate(&srvOpts)
		mutate(&cliOpts)
	}

	srvCh := make(chan *Session, 1)
	errCh := make(chan error, 1)
	go func() {
		s, err := AcceptSession(context.Background(), srvConn, srvOpts)
		if err != nil {
			errCh <- err
			return
		}
		srvCh <- s
	}()

	cli, err := DialSession(context.Background(), cliConn, cliOpts)
	if err != nil {
		t.Fatalf("客户端会话握手失败: %v", err)
	}
	select {
	case srv := <-srvCh:
		t.Cleanup(func() {
			_ = cli.Close()
			_ = srv.Close()
		})
		return cli, srv
	case err := <-errCh:
		t.Fatalf("服务端会话握手失败: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("等待服务端会话超时")
	}
	return nil, nil
}

// readFull 在超时内读满 n 字节，否则判定失败。
func readFull(t *testing.T, r io.Reader, n int, timeout time.Duration) []byte {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, n)
		_, err := io.ReadFull(r, buf)
		ch <- result{buf, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("读取失败: %v", res.err)
		}
		return res.b
	case <-time.After(timeout):
		t.Fatal("读取超时")
		return nil
	}
}

// readExpectEOF 断言读端在超时内返回 EOF（且此前数据已读完）。
func readExpectEOF(t *testing.T, r io.Reader, timeout time.Duration) {
	t.Helper()
	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 64)
		_, err := r.Read(buf)
		ch <- result{err}
	}()
	select {
	case res := <-ch:
		if !errors.Is(res.err, io.EOF) {
			t.Fatalf("err = %v，期望 io.EOF", res.err)
		}
	case <-time.After(timeout):
		t.Fatal("等待 EOF 超时")
	}
}

// readExpectErr 断言读端在超时内返回指定错误。
func readExpectErr(t *testing.T, r io.Reader, want error, timeout time.Duration) {
	t.Helper()
	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 64)
		_, err := r.Read(buf)
		ch <- result{err}
	}()
	select {
	case res := <-ch:
		if !errors.Is(res.err, want) {
			t.Fatalf("err = %v，期望 %v", res.err, want)
		}
	case <-time.After(timeout):
		t.Fatalf("等待 %v 超时", want)
	}
}

func TestStreamDataBothDirections(t *testing.T) {
	cli, srv := newSessionPair(t, nil)

	st, err := cli.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if st.StreamID() != 1 {
		t.Fatalf("首个 StreamID = %d，期望 1", st.StreamID())
	}
	srvSt, err := srv.RegisterStream(st.StreamID())
	if err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}
	if cli.NumStreams() != 1 || srv.NumStreams() != 1 {
		t.Fatalf("流数 client=%d server=%d", cli.NumStreams(), srv.NumStreams())
	}

	toServer := []byte("hello from client")
	if _, err := st.Write(toServer); err != nil {
		t.Fatalf("客户端写: %v", err)
	}
	if got := readFull(t, srvSt, len(toServer), 3*time.Second); !bytes.Equal(got, toServer) {
		t.Fatalf("服务端收到 %q，期望 %q", got, toServer)
	}

	toClient := []byte("hello from server")
	if _, err := srvSt.Write(toClient); err != nil {
		t.Fatalf("服务端写: %v", err)
	}
	if got := readFull(t, st, len(toClient), 3*time.Second); !bytes.Equal(got, toClient) {
		t.Fatalf("客户端收到 %q，期望 %q", got, toClient)
	}
}

func TestStreamLargeWriteIsChunked(t *testing.T) {
	cli, srv := newSessionPair(t, func(o *Options) { o.MaxDataPayload = 4096 })

	st, err := cli.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	srvSt, err := srv.RegisterStream(st.StreamID())
	if err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}

	payload := bytes.Repeat([]byte{0xA5}, 4096*3+123) // 跨多个 DATA 帧
	if _, err := st.Write(payload); err != nil {
		t.Fatalf("写大块: %v", err)
	}
	got := readFull(t, srvSt, len(payload), 5*time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("大块数据不一致: 收到 %d 字节，期望 %d 字节", len(got), len(payload))
	}
}

func TestStreamHalfClose(t *testing.T) {
	cli, srv := newSessionPair(t, nil)

	st, err := cli.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	srvSt, err := srv.RegisterStream(st.StreamID())
	if err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}

	// 客户端发完数据后半关闭：服务端应读完数据再看到 EOF。
	if _, err := st.Write([]byte("part1")); err != nil {
		t.Fatalf("写: %v", err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if got := readFull(t, srvSt, len("part1"), 3*time.Second); string(got) != "part1" {
		t.Fatalf("服务端收到 %q", got)
	}
	readExpectEOF(t, srvSt, 3*time.Second)

	// 客户端写方向已关，再写应报错。
	if _, err := st.Write([]byte("more")); !errors.Is(err, ErrStreamWriteClosed) {
		t.Fatalf("写已关方向 err = %v，期望 ErrStreamWriteClosed", err)
	}

	// 半关闭后服务端仍可回数据（这就是「半」关闭的意义）。
	if _, err := srvSt.Write([]byte("reply")); err != nil {
		t.Fatalf("服务端回写: %v", err)
	}
	if got := readFull(t, st, len("reply"), 3*time.Second); string(got) != "reply" {
		t.Fatalf("客户端收到 %q", got)
	}

	// 服务端也半关闭后，客户端读到 EOF；两个方向都结束后流被回收。
	if err := srvSt.CloseWrite(); err != nil {
		t.Fatalf("服务端 CloseWrite: %v", err)
	}
	readExpectEOF(t, st, 3*time.Second)

	deadline := time.Now().Add(3 * time.Second)
	for (cli.NumStreams() != 0 || srv.NumStreams() != 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cli.NumStreams() != 0 || srv.NumStreams() != 0 {
		t.Fatalf("流未回收: client=%d server=%d", cli.NumStreams(), srv.NumStreams())
	}
}

func TestStreamResetPropagates(t *testing.T) {
	cli, srv := newSessionPair(t, nil)

	st, err := cli.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	srvSt, err := srv.RegisterStream(st.StreamID())
	if err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}

	if err := st.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	readExpectErr(t, srvSt, ErrStreamReset, 3*time.Second)

	deadline := time.Now().Add(3 * time.Second)
	for (cli.NumStreams() != 0 || srv.NumStreams() != 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cli.NumStreams() != 0 || srv.NumStreams() != 0 {
		t.Fatalf("RST 后流未回收: client=%d server=%d", cli.NumStreams(), srv.NumStreams())
	}
	// 会话本身必须存活。
	select {
	case <-cli.Done():
		t.Fatal("RST 流不应该关闭会话")
	default:
	}
}

func TestStreamUnknownStreamIDIgnored(t *testing.T) {
	cli, srv := newSessionPair(t, nil)

	// 未登记的 StreamID：按协议丢弃，不影响会话。
	ctx := context.Background()
	if err := srv.SendFrame(ctx, protocol.TypeData, 999, []byte("orphan")); err != nil {
		t.Fatalf("发送孤儿 DATA: %v", err)
	}
	if err := srv.SendFrame(ctx, protocol.TypeFin, 999, nil); err != nil {
		t.Fatalf("发送孤儿 FIN: %v", err)
	}
	if err := srv.SendFrame(ctx, protocol.TypeRst, 999, nil); err != nil {
		t.Fatalf("发送孤儿 RST: %v", err)
	}

	// 会话仍应正常工作。
	st, err := cli.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	srvSt, err := srv.RegisterStream(st.StreamID())
	if err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}
	if _, err := st.Write([]byte("still alive")); err != nil {
		t.Fatalf("写: %v", err)
	}
	got := readFull(t, srvSt, len("still alive"), 3*time.Second)
	if string(got) != "still alive" {
		t.Fatalf("收到 %q", got)
	}
}

func TestSessionCloseResetsStreams(t *testing.T) {
	cli, srv := newSessionPair(t, nil)

	st, err := cli.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	srvSt, err := srv.RegisterStream(st.StreamID())
	if err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}

	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	readExpectErr(t, srvSt, ErrSessionClosed, 5*time.Second)
}

// TestStreamRecvStallResetsStream 验证：接收缓冲长时间满时重置该流而不是卡死会话。
func TestStreamRecvStallResetsStream(t *testing.T) {
	cli, srv := newSessionPair(t, func(o *Options) {
		o.StreamIdleTimeout = 200 * time.Millisecond
		o.MaxDataPayload = 16 * 1024
	})

	st, err := cli.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	srvSt, err := srv.RegisterStream(st.StreamID())
	if err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}

	// 服务端始终不读：超过 256KB 接收缓冲后，push 超时应把该流重置。
	payload := bytes.Repeat([]byte{0x5A}, 64*1024)
	for i := 0; i < 8; i++ {
		if _, err := st.Write(payload); err != nil {
			break // 被 RST 后写会失败，属预期
		}
	}

	// 等待服务端把这条流重置（从流表中摘除）。
	deadline := time.Now().Add(5 * time.Second)
	for srv.NumStreams() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.NumStreams() != 0 {
		t.Fatal("接收缓冲长时间满，流未被重置")
	}

	// 之后读端应报「缓冲长时间满」而不是一直阻塞。
	var readErr error
	for i := 0; i < 64; i++ {
		buf := make([]byte, 32*1024)
		if _, readErr = srvSt.Read(buf); readErr != nil {
			break
		}
	}
	if !errors.Is(readErr, ErrStreamRecvStalled) {
		t.Fatalf("读端 err = %v，期望 ErrStreamRecvStalled", readErr)
	}

	// 会话本身必须存活。
	select {
	case <-srv.Done():
		t.Fatal("流被拖死不应关闭会话")
	default:
	}
}

func TestOpenStreamRejectedOnServer(t *testing.T) {
	_, srv := newSessionPair(t, nil)
	if _, err := srv.OpenStream(context.Background()); !errors.Is(err, ErrOpenUnsupported) {
		t.Fatalf("err = %v，期望 ErrOpenUnsupported", err)
	}
}

func TestRegisterStreamErrors(t *testing.T) {
	cli, srv := newSessionPair(t, nil)
	if _, err := srv.RegisterStream(0); !errors.Is(err, ErrBadStreamID) {
		t.Fatalf("StreamID=0 err = %v", err)
	}
	st, err := cli.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := srv.RegisterStream(st.StreamID()); err != nil {
		t.Fatalf("首次登记: %v", err)
	}
	if _, err := srv.RegisterStream(st.StreamID()); !errors.Is(err, ErrStreamExists) {
		t.Fatalf("重复登记 err = %v，期望 ErrStreamExists", err)
	}
}

func TestOpenStreamIDIncrementsByTwo(t *testing.T) {
	cli, _ := newSessionPair(t, nil)
	ctx := context.Background()
	for i, want := range []uint32{1, 3, 5} {
		st, err := cli.OpenStream(ctx)
		if err != nil {
			t.Fatalf("第 %d 次 OpenStream: %v", i+1, err)
		}
		if st.StreamID() != want {
			t.Fatalf("StreamID = %d，期望 %d", st.StreamID(), want)
		}
	}
}

func TestOpenStreamRespectsMaxStreams(t *testing.T) {
	cli, _ := newSessionPair(t, func(o *Options) { o.MaxStreams = 2 })
	ctx := context.Background()
	if _, err := cli.OpenStream(ctx); err != nil {
		t.Fatalf("第 1 条: %v", err)
	}
	if _, err := cli.OpenStream(ctx); err != nil {
		t.Fatalf("第 2 条: %v", err)
	}
	if _, err := cli.OpenStream(ctx); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("超出上限 err = %v，期望 ErrTooManyStreams", err)
	}
}

func TestStreamCloseRemovesFromTable(t *testing.T) {
	cli, srv := newSessionPair(t, nil)
	ctx := context.Background()
	st, err := cli.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	srvSt, err := srv.RegisterStream(st.StreamID())
	if err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}
	// 双向都不发 FIN：CloseRead + CloseWrite 各自触发收尾。
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := srvSt.Close(); err != nil {
		t.Fatalf("服务端 Close: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for (cli.NumStreams() != 0 || srv.NumStreams() != 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cli.NumStreams() != 0 || srv.NumStreams() != 0 {
		t.Fatalf("Close 后流未回收: client=%d server=%d", cli.NumStreams(), srv.NumStreams())
	}
}
