package client

import (
	"errors"
	"sync"

	"github.com/LiZeC123/gks/internal/mux"
	"github.com/LiZeC123/gks/internal/protocol"
)

// ErrRouterClosed 表示会话已不可用，等待中的连接应立即失败。
var ErrRouterClosed = errors.New("client: 会话已不可用")

// connectResult 是路由给某条连接的结果（成功响应或会话级失败）。
type connectResult struct {
	resp protocol.ConnectResponse
	err  error
}

// connRouter 把 CONNECT_RESP 按 StreamID 分发给正在等待它的那条连接。
//
// 为什么不能每条连接各注册一个 handler：会话会被连接池复用，旧连接注册的
// handler 仍挂在会话上，新连接的回包可能被投进旧连接的 channel（或旧 handler
// 吞掉新回包）。因此一条会话只注册一个 handler，路由表以 StreamID 为键。
//
// 另注意：dispatch 由会话读循环调用，绝不能阻塞——没有等待者时直接丢弃。
type connRouter struct {
	mu      sync.Mutex
	pending map[uint32]chan connectResult
	closed  bool
}

func newConnRouter() *connRouter {
	return &connRouter{pending: make(map[uint32]chan connectResult)}
}

// dispatch 是注册到会话上的 CONNECT_RESP handler。
func (r *connRouter) dispatch(_ *mux.Session, f protocol.Frame) {
	resp, err := protocol.ParseConnectResponse(f.Payload)
	if err != nil {
		return
	}
	r.mu.Lock()
	ch := r.pending[f.StreamID]
	r.mu.Unlock()
	if ch == nil {
		return // 该流已不在等待（超时/已关闭）
	}
	select {
	case ch <- connectResult{resp: resp}:
	default:
	}
}

// register 登记一条等待中的流，返回只属于它的 channel。
func (r *connRouter) register(streamID uint32) chan connectResult {
	ch := make(chan connectResult, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		ch <- connectResult{err: ErrRouterClosed}
		return ch
	}
	r.pending[streamID] = ch
	return ch
}

// unregister 摘除等待项（幂等）。
func (r *connRouter) unregister(streamID uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, streamID)
}

// close 让所有等待者立刻失败（会话被池丢弃时调用）。
func (r *connRouter) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for id, ch := range r.pending {
		select {
		case ch <- connectResult{err: ErrRouterClosed}:
		default:
		}
		delete(r.pending, id)
	}
}

// pendingCount 返回等待中的连接数（测试与观测用）。
func (r *connRouter) pendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}
