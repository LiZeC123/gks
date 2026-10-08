package transport

import (
	kcp "github.com/xtaci/kcp-go/v5"

	"github.com/LiZeC123/gks/internal/metrics"
)

// SnmpStats 把 kcp-go 的全局统计转成 metrics 需要的形状。
//
// 注意：kcp-go 的 DefaultSnmp 是进程级全局计数器，覆盖本进程的所有 KCP 会话
// （客户端是所有到服务端的会话，服务端是所有被接受的会话），因此它天然适合回答
// 「客户端与服务端之间的 UDP 线速率与重传率是多少」。
func SnmpStats() metrics.TransportStats {
	s := kcp.DefaultSnmp.Copy()
	return metrics.TransportStats{
		UDPBytesSent:     s.OutBytes,
		UDPBytesReceived: s.InBytes,
		OutSegs:          s.OutSegs,
		InSegs:           s.InSegs,
		RetransSegs:      s.RetransSegs,
		FastRetransSegs:  s.FastRetransSegs,
		LostSegs:         s.LostSegs,
		RepeatSegs:       s.RepeatSegs,
		KCPInErrors:      s.KCPInErrors,
	}
}
