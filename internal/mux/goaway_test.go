package mux

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/protocol"
	"github.com/LiZeC123/gks/internal/transport"
)

// waitClose 在超时内等待会话关闭，避免测试挂死。
func waitClose(t *testing.T, s *Session, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatal("等待会话关闭超时")
		return nil
	}
}

// TestCloseWithGoAwayNotifiesPeer 验证「告别」确实到达对端。
//
// 判据是对端的关闭原因：收到 GOAWAY 会走 dispatch → closeWith(ErrSessionClosed)；
// 若 GOAWAY 丢在发送队列里（入队后立刻 closeWith 关掉 conn），对端只会读到 socket 错误。
// 多轮循环用于暴露「select 在 closed 与 sendCh 同时就绪时随机选择」导致的偶发丢帧。
func TestCloseWithGoAwayNotifiesPeer(t *testing.T) {
	const rounds = 50
	for i := 0; i < rounds; i++ {
		cli, srv := newSessionPair(t, nil)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		sendErr := cli.CloseWithGoAway(ctx)
		cancel()

		// 不断言 sendErr：对端收到 GOAWAY 后会立刻关闭连接，本端读循环随之记录 socket 错误，
		// 与「写出结果」存在竞态，两个结果都可能返回。
		if got := waitClose(t, srv, 2*time.Second); !errors.Is(got, ErrSessionClosed) {
			t.Fatalf("第 %d 轮：对端关闭原因 = %v（本端发送返回 %v），期望收到 GOAWAY（ErrSessionClosed）",
				i, got, sendErr)
		}
	}
}

// TestPlainCloseDoesNotNotifyPeer 固定语义差异：Close 不通知对端，对端只能读到 socket 错误。
// 所以「主动放弃一条仍然健康的会话」必须走 CloseWithGoAway，否则对端要等沉默判死。
func TestPlainCloseDoesNotNotifyPeer(t *testing.T) {
	cli, srv := newSessionPair(t, nil)

	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := waitClose(t, srv, 2*time.Second)
	if errors.Is(got, ErrSessionClosed) {
		t.Fatal("普通 Close 不应被对端识别为 GOAWAY")
	}
	if got == nil {
		t.Fatal("对端应因读到 socket 错误而关闭并记录原因")
	}
}

// gatedConn 在 arm 之后挂住 Write，用于验证「等待上限」与「关闭一定会发生」。
type gatedConn struct {
	net.Conn
	mu    sync.Mutex
	armed bool
	gate  chan struct{}
}

func (c *gatedConn) arm() {
	c.mu.Lock()
	c.armed = true
	c.mu.Unlock()
}

func (c *gatedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	armed, gate := c.armed, c.gate
	c.mu.Unlock()
	if armed {
		<-gate
	}
	return c.Conn.Write(p)
}

// newClientWrappedPair 建立一对已认证会话，客户端连接先经过 wrap 包装（用于注入行为）。
func newClientWrappedPair(t *testing.T, wrap func(net.Conn) net.Conn) (*Session, *Session) {
	t.Helper()
	cliPipe, srvPipe := net.Pipe()
	srvCh := make(chan *Session, 1)
	go func() {
		s, err := AcceptSession(context.Background(), srvPipe, serverOptions(newReplayCache(t), testPSK, 0))
		if err != nil {
			return
		}
		srvCh <- s
	}()
	cli, err := DialSession(context.Background(), wrap(cliPipe), clientOptions(testPSK, 0))
	if err != nil {
		t.Fatalf("DialSession: %v", err)
	}
	srv := <-srvCh
	t.Cleanup(func() {
		_ = cli.Close()
		_ = srv.Close()
	})
	return cli, srv
}

// TestCloseWithGoAwayBoundedByContext 验证：GOAWAY 写不出去时，CloseWithGoAway 在 ctx 上限
// 返回，且无论写出成败会话都必须已经关闭（不能把会话留着）。
func TestCloseWithGoAwayBoundedByContext(t *testing.T) {
	gated := &gatedConn{gate: make(chan struct{})}
	defer close(gated.gate) // 放行被挂住的写循环
	cli, _ := newClientWrappedPair(t, func(c net.Conn) net.Conn {
		gated.Conn = c
		return gated
	})

	gated.arm() // 此后所有写入都会挂住
	err := error(nil)

	const limit = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	start := time.Now()
	err = cli.CloseWithGoAway(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("写出被挂住时应返回 ctx 超时，实际 %v", err)
	}
	if elapsed < limit {
		t.Fatalf("早于 ctx 上限返回: %s", elapsed)
	}
	if elapsed > limit+2*time.Second {
		t.Fatalf("等待明显超出上限: %s", elapsed)
	}
	select {
	case <-cli.Done():
	default:
		t.Fatal("即使 GOAWAY 没写出去，会话也必须已关闭")
	}
}

// TestSendSyncOnClosedSessionFailsFast 验证已关闭会话上的同步发送立刻失败而不是挂住。
func TestSendSyncOnClosedSessionFailsFast(t *testing.T) {
	cli, srv := newSessionPair(t, nil)
	defer func() { _ = srv.Close() }()
	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- cli.sendSync(ctx, protocol.Frame{Type: protocol.TypeGoAway})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("会话已关闭时同步发送应当报错")
		}
	case <-time.After(time.Second):
		t.Fatal("同步发送在已关闭会话上挂住了")
	}
}

// TestCloseWithGoAwayOverKCP 在真实 KCP/UDP 上验证告别能到达对端（net.Pipe 与 UDP 行为不同）。
func TestCloseWithGoAwayOverKCP(t *testing.T) {
	ln := mustListen(t)
	srvCh := make(chan *Session, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		s, err := AcceptSession(context.Background(), conn, serverOptions(newReplayCache(t), testPSK, 0))
		if err != nil {
			return
		}
		srvCh <- s
	}()

	conn, err := transport.NewDialer(testTransportOptions()).Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	cli, err := DialSession(context.Background(), conn, clientOptions(testPSK, 0))
	if err != nil {
		t.Fatalf("DialSession: %v", err)
	}
	srv := <-srvCh
	defer func() { _ = srv.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sendErr := cli.CloseWithGoAway(ctx)
	got := waitClose(t, srv, 3*time.Second)
	t.Logf("本端发送返回: %v；对端关闭原因: %v", sendErr, got)
	if !errors.Is(got, ErrSessionClosed) {
		t.Fatalf("真实 KCP 上对端关闭原因 = %v，期望 ErrSessionClosed（GOAWAY 未到达）", got)
	}
}

// drainerConn 实现 gracefulDrainer：CloseAfterDrain 会被延迟，用于观察排空契约。
type drainerConn struct {
	net.Conn
	delay  time.Duration
	mu     sync.Mutex
	calls  int
	closed chan struct{}
}

func (c *drainerConn) CloseAfterDrain(d time.Duration) error {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	close(c.closed)
	return c.Conn.Close()
}

// TestGracefulDrainContract 固定优雅关闭的排空契约：
// 会话逻辑关闭立即返回（不阻塞热路径），WaitDrained 等到传输层真正释放连接后才返回。
func TestGracefulDrainContract(t *testing.T) {
	const drain = 150 * time.Millisecond
	dc := &drainerConn{delay: drain, closed: make(chan struct{})}
	cli, _ := newClientWrappedPair(t, func(c net.Conn) net.Conn {
		dc.Conn = c
		return dc
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	cli.closeWithGracefulConn(ErrSessionClosed)
	if blocked := time.Since(start); blocked > drain/2 {
		t.Fatalf("优雅关闭阻塞了调用方：%s（排空应当异步）", blocked)
	}
	if err := cli.WaitDrained(ctx); err != nil {
		t.Fatalf("WaitDrained: %v", err)
	}
	if elapsed := time.Since(start); elapsed < drain {
		t.Fatalf("WaitDrained 早于排空完成就返回：%s < %s", elapsed, drain)
	}
	dc.mu.Lock()
	calls := dc.calls
	dc.mu.Unlock()
	if calls != 1 {
		t.Fatalf("CloseAfterDrain 调用次数 = %d，期望 1", calls)
	}
	select {
	case <-dc.closed:
	default:
		t.Fatal("排空结束后底层连接应已关闭")
	}
}
