package mux

import (
	"context"
	"io"
	"net"
	"sync"
)

// closeWriter 是支持半关闭的连接（TCP 连接满足）。
type closeWriter interface{ CloseWrite() error }

// Bridge 在 net.Conn 与 Stream 之间双向转发，并实现半关闭语义（dev.md §3.8）：
//
//   - 本地读端 EOF → stream.CloseWrite()（发 FIN），仍继续接收对端数据；
//   - 收到对端 FIN（stream 读端 EOF）→ 关闭本地连接的写方向；
//   - 任一方向出错 → 对应方向 Reset/Close，避免连接悬挂；
//   - ctx 取消 → 强制中断两侧。
//
// 返回第一个非 EOF 错误；正常结束返回 nil。
func Bridge(ctx context.Context, conn net.Conn, st Stream, bufSize int) error {
	if bufSize <= 0 {
		bufSize = 32 * 1024
	}

	done := make(chan struct{})
	var finishOnce sync.Once
	finish := func() { finishOnce.Do(func() { close(done) }) }

	// ctx 取消时强制中断，保证两个 copy goroutine 一定能退出。
	go func() {
		select {
		case <-ctx.Done():
			_ = st.Reset()
			_ = conn.Close()
		case <-done:
		}
	}()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	record := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

	// 本地 → 远端
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, bufSize)
		if _, err := io.CopyBuffer(st, conn, buf); err != nil {
			record(err)
			_ = st.Reset()
			return
		}
		// 本地不再发送：发 FIN，接收方向继续。
		record(st.CloseWrite())
	}()

	// 远端 → 本地
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, bufSize)
		if _, err := io.CopyBuffer(conn, st, buf); err != nil {
			record(err)
			_ = conn.Close()
			return
		}
		// 对端不再发送：关闭本地写方向。
		closeWrite(conn)
	}()

	wg.Wait()
	finish()
	return firstErr
}

// closeWrite 尽力半关闭本地连接；不支持半关闭的连接（如 net.Pipe）只能整体关闭。
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}
