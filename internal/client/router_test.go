package client

import (
	"testing"

	"github.com/LiZeC123/gks/internal/protocol"
)

func frameFor(streamID uint32, rep byte) protocol.Frame {
	resp, err := protocol.ConnectResponse{Reply: rep, Bind: protocol.UnspecificBind()}.Marshal()
	if err != nil {
		panic(err)
	}
	return protocol.Frame{Type: protocol.TypeConnectResp, StreamID: streamID, Payload: resp}
}

func TestConnRouterRoutesByStreamID(t *testing.T) {
	r := newConnRouter()
	ch1 := r.register(1)
	ch3 := r.register(3)
	ch5 := r.register(5)

	// 故意乱序投递，并且多投一次重复回包。
	r.dispatch(nil, frameFor(3, protocol.RepConnectionRefused))
	r.dispatch(nil, frameFor(3, protocol.RepConnectionRefused))
	r.dispatch(nil, frameFor(5, protocol.RepSuccess))
	r.dispatch(nil, frameFor(1, protocol.RepHostUnreachable))

	for _, tc := range []struct {
		id   uint32
		ch   chan connectResult
		want byte
	}{
		{1, ch1, protocol.RepHostUnreachable},
		{3, ch3, protocol.RepConnectionRefused},
		{5, ch5, protocol.RepSuccess},
	} {
		select {
		case res := <-tc.ch:
			if res.err != nil {
				t.Fatalf("流 %d 收到错误: %v", tc.id, res.err)
			}
			if res.resp.Reply != tc.want {
				t.Fatalf("流 %d 的 REP = %s，期望 %s", tc.id, protocol.RepString(res.resp.Reply), protocol.RepString(tc.want))
			}
		default:
			t.Fatalf("流 %d 没有收到回包", tc.id)
		}
	}
	if n := r.pendingCount(); n != 3 {
		t.Fatalf("等待中的连接数 = %d，期望 3", n)
	}
}

func TestConnRouterIgnoresUnknownAndUnparsable(t *testing.T) {
	r := newConnRouter()
	ch1 := r.register(1)

	// 没有等待者的 StreamID：直接丢弃，不阻塞。
	r.dispatch(nil, frameFor(99, protocol.RepSuccess))
	// 解析失败的 payload：忽略。
	r.dispatch(nil, protocol.Frame{Type: protocol.TypeConnectResp, StreamID: 1, Payload: []byte{0x00}})
	// 非法 StreamID 帧（非 CONNECT_RESP 类型也会被路由表忽略）。
	r.dispatch(nil, protocol.Frame{Type: protocol.TypeData, StreamID: 1, Payload: []byte("x")})

	select {
	case res := <-ch1:
		if res.err != nil {
			t.Fatalf("不应收到错误: %v", res.err)
		}
		t.Fatalf("不应收到回包: %+v", res.resp)
	default:
	}
}

func TestConnRouterUnregisterStopsDelivery(t *testing.T) {
	r := newConnRouter()
	ch1 := r.register(1)
	r.unregister(1)
	if n := r.pendingCount(); n != 0 {
		t.Fatalf("摘除后等待数 = %d", n)
	}
	r.dispatch(nil, frameFor(1, protocol.RepSuccess))
	select {
	case res := <-ch1:
		t.Fatalf("摘除后不应再收到回包: %+v", res)
	default:
	}
}

func TestConnRouterCloseFailsWaiters(t *testing.T) {
	r := newConnRouter()
	ch1 := r.register(1)
	ch2 := r.register(3)
	r.close()

	for _, ch := range []chan connectResult{ch1, ch2} {
		select {
		case res := <-ch:
			if res.err != ErrRouterClosed {
				t.Fatalf("err = %v，期望 ErrRouterClosed", res.err)
			}
		default:
			t.Fatal("close 后等待者应立即失败")
		}
	}
	// 关闭后新登记也应立即失败，而不是永久阻塞。
	ch3 := r.register(5)
	select {
	case res := <-ch3:
		if res.err != ErrRouterClosed {
			t.Fatalf("err = %v，期望 ErrRouterClosed", res.err)
		}
	default:
		t.Fatal("关闭后登记应立即失败")
	}
	// 重复 close 幂等。
	r.close()
}
