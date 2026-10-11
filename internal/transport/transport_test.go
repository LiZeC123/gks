package transport

import (
	"testing"
	"time"
)

// TestListenerSetReadDeadlineUnblocksAccept 验证优雅退出使用的关键能力：
// 用读期限中止 Accept，而不关闭与其他会话共用的监听 socket。
func TestListenerSetReadDeadlineUnblocksAccept(t *testing.T) {
	ln, err := Listen("127.0.0.1:0", Options{
		Interval: 10 * time.Millisecond,
		MTU:      1350,
		SndWnd:   256,
		RcvWnd:   256,
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	if err := ln.SetReadDeadline(time.Now()); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Accept 应当因读期限而失败")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SetReadDeadline 未能中止 Accept")
	}
}
