package mux

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/metrics"
	"github.com/LiZeC123/gks/internal/protocol"
)

// newSessionPairWith 建立一对已认证会话，允许分别修改两端选项。
//
// 与 stream_test.go 的 newSessionPair 的区别：那个 helper 把同一个 mutate 应用到两端；
// 被动判活只应在服务端启用（客户端有自己的心跳判死），因此需要分开设置。
func newSessionPairWith(t *testing.T, srvMutate, cliMutate func(*Options)) (*Session, *Session) {
	t.Helper()
	cliConn, srvConn := net.Pipe()

	srvOpts := serverOptions(newReplayCache(t), testPSK, 0)
	cliOpts := clientOptions(testPSK, 0)
	if srvMutate != nil {
		srvMutate(&srvOpts)
	}
	if cliMutate != nil {
		cliMutate(&cliOpts)
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

// TestPeerSilenceReapsSession 验证：对端沉默超过阈值后，服务端主动回收会话，并连带清理其上的流。
func TestPeerSilenceReapsSession(t *testing.T) {
	srvReg := &metrics.Registry{}
	cli, srv := newSessionPairWith(t, func(o *Options) {
		o.PeerSilenceTimeout = 150 * time.Millisecond
		o.Metrics = srvReg
	}, nil)

	st, err := cli.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	srvSt, err := srv.RegisterStream(st.StreamID())
	if err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}

	// 之后客户端不再发送任何帧 → 服务端应在 150ms + 一个检查周期内判死。
	got := waitClose(t, srv, 3*time.Second)
	if got != ErrSessionSilence {
		t.Fatalf("服务端关闭原因 = %v，期望 %v", got, ErrSessionSilence)
	}
	// 会话关闭等于向所有流广播 RST：流不能被漏掉。
	readExpectErr(t, srvSt, ErrSessionClosed, 2*time.Second)
	// 观测：被判活回收的会话必须计入 sessions.reaped_silence_total。
	if got := srvReg.SessionsReapedSilence.Load(); got != 1 {
		t.Fatalf("SessionsReapedSilence = %d，期望 1", got)
	}

	// 客户端侧没有开启被动判活，不应被本地循环关掉（是服务端关的连接）。
	select {
	case <-cli.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("等待客户端感知连接关闭超时")
	}
}

// TestPeerSilenceKeepsLiveSession 验证判活口径：任何完整入站帧都算活（不只是 PING），
// 只要对端还在说话就不该被回收；停止说话后才回收。
func TestPeerSilenceKeepsLiveSession(t *testing.T) {
	const timeout = 300 * time.Millisecond
	cli, srv := newSessionPairWith(t, func(o *Options) {
		o.PeerSilenceTimeout = timeout
	}, nil)

	st, err := cli.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	// 阶段一：控制帧（PING）持续 500ms
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := cli.SendControl(ctx, protocol.TypePing, []byte("01234567")); err != nil {
			cancel()
			t.Fatalf("发送 PING: %v", err)
		}
		cancel()
		time.Sleep(100 * time.Millisecond)
	}
	// 阶段二：数据帧持续 500ms（此时没有任何回程流量，只有 DATA 在证明对端活着）
	var lastSend time.Time
	for i := 0; i < 5; i++ {
		if _, err := st.Write([]byte("still here")); err != nil {
			t.Fatalf("写 DATA: %v", err)
		}
		lastSend = time.Now()
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case <-srv.Done():
		t.Fatalf("持续有入站帧时被误判沉默：%v", srv.Wait())
	default:
	}

	// 阶段三：停止发送 → 应在一个阈值 + 检查周期内回收（计时从最后一次发出算起）
	if got := waitClose(t, srv, 3*time.Second); got != ErrSessionSilence {
		t.Fatalf("服务端关闭原因 = %v，期望 %v", got, ErrSessionSilence)
	}
	elapsed := time.Since(lastSend)
	if elapsed < timeout {
		t.Fatalf("早于阈值就被回收：%s < %s", elapsed, timeout)
	}
	t.Logf("最后一次入站帧之后 %s 被回收（阈值 %s，检查周期 %s）", elapsed, timeout, timeout/4)
}

// TestPeerSilenceDisabled 验证默认关闭：不设置阈值时，长时间沉默也不会被回收。
func TestPeerSilenceDisabled(t *testing.T) {
	_, srv := newSessionPairWith(t, nil, nil)
	time.Sleep(400 * time.Millisecond)
	select {
	case <-srv.Done():
		t.Fatalf("未启用被动判活时不应回收会话：%v", srv.Wait())
	default:
	}
}
