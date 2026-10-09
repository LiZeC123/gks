// Package metrics 提供进程级计数器、1s 粒度速率历史与统计呈现（控制台表格 / HTTP JSON）。
//
// 三层数据来源：
//
//   - 流/会话层：本包的 Registry，由 mux 层累加（载荷字节、活跃会话/流数）；
//   - 传输层：kcp-go 的全局 Snmp（UDP 字节、KCP 段、重传/丢包），
//     由调用方通过 Sources 的 transport hook 注入，本包不依赖 kcp-go；
//   - 错误计数：认证失败、会话建立失败、SOCKS5 失败、目标拨号失败。
//
// 速率是「窗口两端的累计量之差 ÷ 窗口长度」：Collector 把快照按 Resolution(1s) 写入
// 环形历史，任意窗口（1s ~ Retention）内的速率都由历史两端即时算出，因此外部程序
// 按 1s 或 30s 拉取都能拿到对应窗口的真实值。
package metrics

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Registry 是进程级计数器。全部字段使用原子操作，可被多个 goroutine 并发更新。
type Registry struct {
	// SessionsActive / StreamsActive 是实时数量（会增减）。
	SessionsActive atomic.Int64
	StreamsActive  atomic.Int64
	// SessionsTotal / StreamsTotal 是累计数量（只增）。
	SessionsTotal atomic.Uint64
	StreamsTotal  atomic.Uint64

	// PayloadSent / PayloadReceived 是隧道载荷字节数：
	// Sent 表示本进程发往对端的应用数据，Received 表示从对端收到的应用数据。
	PayloadSent     atomic.Uint64
	PayloadReceived atomic.Uint64

	// 错误计数
	AuthFailures        atomic.Uint64 // 认证握手失败
	SessionDialFailures atomic.Uint64 // 建立/认证 KCP 会话失败（含服务端不可达）
	Socks5Failures      atomic.Uint64 // 本地 SOCKS5 握手或请求不合法
	DialFailures        atomic.Uint64 // 服务端拨号目标失败
}

// Default 是进程默认注册表。
var Default = &Registry{}

// TransportStats 是从传输层采集的统计（由调用方从 kcp-go Snmp 填充）。
type TransportStats struct {
	UDPBytesSent     uint64
	UDPBytesReceived uint64
	OutSegs          uint64
	InSegs           uint64
	RetransSegs      uint64
	FastRetransSegs  uint64
	LostSegs         uint64
	RepeatSegs       uint64
	KCPInErrors      uint64
	// FEC 相关（未启用 FEC 时恒为 0）
	FECRecovered uint64
	FECErrs      uint64
}

// PoolStats 是会话池的关键状态（由调用方注入，metrics 不依赖 client 包）。
type PoolStats struct {
	Sessions int
	InUse    int
	Idle     int
	Creating int
	Waiters  int
	Rebuilds uint64
}

// RegistrySnapshot 是注册表在某一时刻的取值。
type RegistrySnapshot struct {
	SessionsActive      int64
	StreamsActive       int64
	SessionsTotal       uint64
	StreamsTotal        uint64
	PayloadSent         uint64
	PayloadReceived     uint64
	AuthFailures        uint64
	SessionDialFailures uint64
	Socks5Failures      uint64
	DialFailures        uint64
}

// Snapshot 是注册表 + 传输层 + 会话池的完整快照。
type Snapshot struct {
	At        time.Time
	Registry  RegistrySnapshot
	Transport TransportStats
	Pool      PoolStats
}

// Take 采集一次快照。
func (r *Registry) Take(transport TransportStats, pool PoolStats, now time.Time) Snapshot {
	if r == nil {
		r = Default
	}
	return Snapshot{
		At: now,
		Registry: RegistrySnapshot{
			SessionsActive:      r.SessionsActive.Load(),
			StreamsActive:       r.StreamsActive.Load(),
			SessionsTotal:       r.SessionsTotal.Load(),
			StreamsTotal:        r.StreamsTotal.Load(),
			PayloadSent:         r.PayloadSent.Load(),
			PayloadReceived:     r.PayloadReceived.Load(),
			AuthFailures:        r.AuthFailures.Load(),
			SessionDialFailures: r.SessionDialFailures.Load(),
			Socks5Failures:      r.Socks5Failures.Load(),
			DialFailures:        r.DialFailures.Load(),
		},
		Transport: transport,
		Pool:      pool,
	}
}

// Sample 是一个窗口内的速率与增量（窗口两端 = Current 与窗口起点）。
type Sample struct {
	Interval time.Duration
	Current  Snapshot

	// 应用层（隧道载荷）速率，单位 B/s
	PayloadSentBps float64
	PayloadRecvBps float64
	// 传输层（UDP）速率，单位 B/s
	WireSentBps float64
	WireRecvBps float64

	// RetransRate 是本次区间内「重传段 / 发送段」，可看作链路丢包的代理指标。
	RetransRate       float64
	OutSegsDelta      uint64
	LostSegsDelta     uint64
	RepeatSegsDelta   uint64
	KCPInErrorsDelta  uint64
	FECRecoveredDelta uint64
	FECErrsDelta      uint64
}

// Sources 提供外部数据来源（传输层与会话池）；为 nil 时对应数据按 0 处理。
type Sources struct {
	Transport func() TransportStats
	Pool      func() PoolStats
}

// computeSample 计算 prev → cur 之间的速率与增量。它是纯函数，便于单测；
// 窗口的选取（历史环形缓冲）由 Collector 负责。
func computeSample(prev, cur Snapshot) Sample {
	smp := Sample{Current: cur}
	smp.Interval = cur.At.Sub(prev.At)
	if smp.Interval <= 0 {
		return smp
	}
	secs := smp.Interval.Seconds()
	smp.PayloadSentBps = float64(sub(cur.Registry.PayloadSent, prev.Registry.PayloadSent)) / secs
	smp.PayloadRecvBps = float64(sub(cur.Registry.PayloadReceived, prev.Registry.PayloadReceived)) / secs
	smp.WireSentBps = float64(sub(cur.Transport.UDPBytesSent, prev.Transport.UDPBytesSent)) / secs
	smp.WireRecvBps = float64(sub(cur.Transport.UDPBytesReceived, prev.Transport.UDPBytesReceived)) / secs
	smp.LostSegsDelta = sub(cur.Transport.LostSegs, prev.Transport.LostSegs)
	smp.RepeatSegsDelta = sub(cur.Transport.RepeatSegs, prev.Transport.RepeatSegs)
	smp.KCPInErrorsDelta = sub(cur.Transport.KCPInErrors, prev.Transport.KCPInErrors)
	smp.FECRecoveredDelta = sub(cur.Transport.FECRecovered, prev.Transport.FECRecovered)
	smp.FECErrsDelta = sub(cur.Transport.FECErrs, prev.Transport.FECErrs)

	smp.OutSegsDelta = sub(cur.Transport.OutSegs, prev.Transport.OutSegs)
	if smp.OutSegsDelta > 0 {
		smp.RetransRate = float64(sub(cur.Transport.RetransSegs, prev.Transport.RetransSegs)) / float64(smp.OutSegsDelta)
	}
	return smp
}

// minRetransSampleSegs 是计算重传率所需的最小发送段数：
// 样本太小时（例如只发了两段、重传两段）百分比没有意义且会误导。
const minRetransSampleSegs = 20

// retransText 把重传率渲染成可读文本；样本太小或没有速率时返回 n/a。
func retransText(smp Sample) string {
	if smp.OutSegsDelta < minRetransSampleSegs {
		return "n/a"
	}
	return fmt.Sprintf("%.2f%%", smp.RetransRate*100)
}

// sub 计算无符号增量；计数器被重置时不返回负数。
func sub(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

// HumanBytes 把字节数格式化成便于阅读的字符串。
func HumanBytes(b float64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%.0f B", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.1f KB", b/1024)
	case b < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", b/(1024*1024))
	default:
		return fmt.Sprintf("%.2f GB", b/(1024*1024*1024))
	}
}

// HumanRate 把 B/s 格式化成便于阅读的字符串。
func HumanRate(bps float64) string {
	switch {
	case bps < 1024:
		return fmt.Sprintf("%.0f B/s", bps)
	case bps < 1024*1024:
		return fmt.Sprintf("%.1f KB/s", bps/1024)
	case bps < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB/s", bps/(1024*1024))
	default:
		return fmt.Sprintf("%.2f GB/s", bps/(1024*1024*1024))
	}
}
