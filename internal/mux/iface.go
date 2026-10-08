// Package mux 定义 Session 上的多路复用抽象与自研实现。
//
// 接口先行的目的：把「流」的语义（含半关闭）与「怎么在一条会话上复用」解耦，
// 便于日后替换为 xtaci/smux（注意 smux 的半关闭语义不完整，替换时 CloseWrite
// 只能降级为 RST 全关）。见 dev.md §3.6。
package mux

import (
	"context"
	"errors"
	"io"
)

// 多路复用相关错误。
var (
	// ErrMuxClosed 表示多路复用器已关闭。
	ErrMuxClosed = errors.New("mux: 会话已关闭")
	// ErrOpenUnsupported 表示该端不支持主动开流（Server 侧）。
	ErrOpenUnsupported = errors.New("mux: 该端不支持主动开流")
	// ErrAcceptUnsupported 表示该端不支持接受流（Client 侧）。
	ErrAcceptUnsupported = errors.New("mux: 该端不支持接受流")
	// ErrTooManyStreams 表示已达单会话流数上限。
	ErrTooManyStreams = errors.New("mux: 流数已达上限")
	// ErrStreamClosed 表示流已关闭。
	ErrStreamClosed = errors.New("mux: 流已关闭")
)

// Stream 是一条逻辑流，语义对齐 net.Conn 并额外提供半关闭。
type Stream interface {
	io.ReadWriteCloser

	// StreamID 返回该流的标识。
	StreamID() uint32
	// CloseWrite 发送 FIN：本方向不再发送 DATA，但仍可接收。
	CloseWrite() error
	// CloseRead 关闭本地读方向：对端仍可发送，但本地不再读取。
	CloseRead() error
	// Reset 发送 RST：立即双向关闭并丢弃缓冲。
	Reset() error
}

// Mux 是一条 Session 上的多路复用器。
type Mux interface {
	// OpenStream 主动开一条流（Client 侧）。
	OpenStream(ctx context.Context) (Stream, error)
	// AcceptStream 接受对端开出来的流（Server 侧）。
	AcceptStream(ctx context.Context) (Stream, error)
	// NumStreams 返回当前活跃流数。
	NumStreams() int
	// Close 关闭多路复用器与其下所有流。
	Close() error
	// CloseChan 在关闭后关闭，便于 select 等待。
	CloseChan() <-chan struct{}
}
