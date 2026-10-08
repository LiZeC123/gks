// Package metrics 提供进程级计数器与周期采样，用于观察 gks 的传输速率与运行状态。
//
// 三层数据来源：
//
//   - 流/会话层：本包的 Registry，由 mux 层累加（载荷字节、活跃会话/流数）；
//   - 传输层：kcp-go 的全局 Snmp（UDP 字节、KCP 段、重传/丢包），
//     由调用方通过 Sampler 的 transport hook 注入，本包不依赖 kcp-go；
//   - 错误计数：认证失败、会话建立失败、SOCKS5 失败、目标拨号失败。
//
// 速率是「两次采样之间的增量 ÷ 间隔」，因此需要连续两次 Sample 才有意义。
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LiZeC123/gks/internal/log"
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

// Snapshot 是注册表 + 传输层的完整快照。
type Snapshot struct {
	At        time.Time
	Registry  RegistrySnapshot
	Transport TransportStats
}

// Take 采集一次快照。
func (r *Registry) Take(transport TransportStats, now time.Time) Snapshot {
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
	}
}

// Sample 是两次采样之间的速率与增量。
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
	RetransRate      float64
	OutSegsDelta     uint64
	LostSegsDelta    uint64
	RepeatSegsDelta  uint64
	KCPInErrorsDelta uint64
}

// Sampler 周期性地采集快照并计算速率。
type Sampler struct {
	reg       *Registry
	transport func() TransportStats
	interval  time.Duration
	logger    *slog.Logger

	mu   sync.Mutex
	prev *Snapshot
}

// NewSampler 构造采样器。transport 为 nil 时传输层数据全为 0。
func NewSampler(reg *Registry, interval time.Duration, logger *slog.Logger, transport func() TransportStats) *Sampler {
	if reg == nil {
		reg = Default
	}
	if transport == nil {
		transport = func() TransportStats { return TransportStats{} }
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Sampler{reg: reg, interval: interval, logger: logger, transport: transport}
}

// Interval 返回采样间隔。
func (s *Sampler) Interval() time.Duration { return s.interval }

// Sample 用当前时间采一次样并计算速率。
func (s *Sampler) Sample() Sample { return s.sampleAt(time.Now()) }

func (s *Sampler) sampleAt(now time.Time) Sample {
	cur := s.reg.Take(s.transport(), now)

	s.mu.Lock()
	prev := s.prev
	s.prev = &cur
	s.mu.Unlock()

	smp := Sample{Current: cur}
	if prev == nil {
		// 首次采样没有基准，速率与增量都为 0。
		return smp
	}
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

	smp.OutSegsDelta = sub(cur.Transport.OutSegs, prev.Transport.OutSegs)
	if smp.OutSegsDelta > 0 {
		smp.RetransRate = float64(sub(cur.Transport.RetransSegs, prev.Transport.RetransSegs)) / float64(smp.OutSegsDelta)
	}
	return smp
}

// Run 阻塞直到 ctx 结束，每 interval 记录一行统计；interval <= 0 时直接返回。
func (s *Sampler) Run(ctx context.Context) {
	if s.interval <= 0 {
		return
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Log(s.Sample())
		}
	}
}

// Log 把一次采样结果打成一行结构化日志。
func (s *Sampler) Log(smp Sample) {
	r := smp.Current.Registry
	s.logger.Info("传输统计",
		log.Event, "metrics",
		"interval", smp.Interval.String(),
		"sessions", r.SessionsActive,
		"streams", r.StreamsActive,
		"sessions_total", r.SessionsTotal,
		"streams_total", r.StreamsTotal,
		"payload_sent", HumanRate(smp.PayloadSentBps),
		"payload_recv", HumanRate(smp.PayloadRecvBps),
		"wire_sent", HumanRate(smp.WireSentBps),
		"wire_recv", HumanRate(smp.WireRecvBps),
		"payload_sent_total", HumanBytes(float64(r.PayloadSent)),
		"payload_recv_total", HumanBytes(float64(r.PayloadReceived)),
		"wire_sent_total", HumanBytes(float64(smp.Current.Transport.UDPBytesSent)),
		"wire_recv_total", HumanBytes(float64(smp.Current.Transport.UDPBytesReceived)),
		"retrans", retransField(smp),
		"lost_segs", smp.LostSegsDelta,
		"repeat_segs", smp.RepeatSegsDelta,
		"kcp_in_errors", smp.KCPInErrorsDelta,
		"errors", fmt.Sprintf("auth=%d session=%d socks5=%d dial=%d",
			r.AuthFailures, r.SessionDialFailures, r.Socks5Failures, r.DialFailures),
	)
}

// minRetransSampleSegs 是计算重传率所需的最小发送段数：
// 样本太小时（例如只发了两段、重传两段）百分比没有意义且会误导。
const minRetransSampleSegs = 20

func retransField(smp Sample) string {
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
