// Package dashboard 是独立于 gks 主程序的监控面板：
// 周期拉取 gks 的 /metrics 端点，把样本存进本地环，并以网页形式展示累计量、速率与趋势。
//
// 设计要点：
//   - 只通过 HTTP 与 gks 通信，不读它的配置、不共享进程，gks 的启停完全不影响面板；
//   - 拉不到数据时容忍（保留历史、标红提示），面板自己的 HTTP 服务保持可用；
//   - 页面与静态资源全部用 go:embed 打进二进制，单文件即可分发。
package dashboard

import (
	"sync"
	"time"

	"github.com/LiZeC123/gks/internal/metrics"
)

// Sample 是一次成功拉取的观测。
type Sample struct {
	// At 是上游响应里的 now（上游自己的时钟）；缺失时回退为 Received。
	At time.Time
	// Received 是面板收到响应的时刻，也是面板侧的时间轴。
	Received time.Time
	// Metrics 是上游 /metrics 的原始 payload（JSON 契约见 internal/metrics.Payload）。
	Metrics metrics.Payload

	// ErrorsDelta / StreamsDelta 是相对上一条样本的累计增量（首条为 0）。
	// 上游只给累计错误数，窗口错误率只能由面板自己按相邻样本差分，因此必须随样本一起存。
	ErrorsDelta  uint64
	StreamsDelta uint64
}

// ErrorsPer1k 返回窗口内「每千条流的错误数」；窗口内没有新流时为 nil（曲线断开）。
func (s Sample) ErrorsPer1k() *float64 {
	if s.StreamsDelta == 0 {
		return nil
	}
	v := float64(s.ErrorsDelta) / float64(s.StreamsDelta) * 1000
	return &v
}

// RetransRatio 返回上游窗口内的重传率；窗口样本不足或不可用时为 nil。
func (s Sample) RetransRatio() *float64 {
	w := s.Metrics.Link.Window
	if !w.RetransRatioAvailable || w.RetransRatio == nil {
		return nil
	}
	return w.RetransRatio
}

// TotalErrors 返回四类错误计数之和。
func TotalErrors(p metrics.Payload) uint64 {
	return p.Errors.Auth + p.Errors.Session + p.Errors.Socks5 + p.Errors.Dial
}

// State 是面板的共享状态：样本环 + 上游可达性。所有方法并发安全。
type State struct {
	mu       sync.RWMutex
	capacity int
	samples  []Sample

	lastAttempt time.Time
	lastSuccess time.Time
	lastError   string
	reachable   bool
	schemaWarn  string
	role        string
}

// NewState 构造状态；capacity 是保留的样本条数。
func NewState(capacity int) *State {
	if capacity < 1 {
		capacity = 1
	}
	return &State{capacity: capacity}
}

// Capacity 返回保留的样本条数。
func (s *State) Capacity() int { return s.capacity }

// MarkSuccess 记录一次成功拉取：与上一条样本差分出窗口增量，然后追加进环。
// schemaWarn 非空时（上游 schema_version 不匹配）只做提示，数据照常使用。
func (s *State) MarkSuccess(smp Sample, schemaWarn string) {
	if smp.Received.IsZero() {
		smp.Received = time.Now()
	}
	if smp.At.IsZero() {
		smp.At = smp.Received
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.samples); n > 0 {
		prev := s.samples[n-1]
		smp.ErrorsDelta = counterSub(TotalErrors(smp.Metrics), TotalErrors(prev.Metrics))
		smp.StreamsDelta = counterSub(smp.Metrics.Streams.Total, prev.Metrics.Streams.Total)
	}
	s.samples = append(s.samples, smp)
	if len(s.samples) > s.capacity {
		n := copy(s.samples, s.samples[len(s.samples)-s.capacity:])
		s.samples = s.samples[:n]
	}
	s.lastAttempt = smp.Received
	s.lastSuccess = smp.Received
	s.lastError = ""
	s.reachable = true
	s.schemaWarn = schemaWarn
	if smp.Metrics.Role != "" {
		s.role = smp.Metrics.Role
	}
}

// MarkFailure 记录一次失败拉取：保留已有样本与历史，只更新可达性与错误信息。
func (s *State) MarkFailure(at time.Time, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAttempt = at
	s.lastError = errMsg
	s.reachable = false
}

// Snapshot 返回一份不可变的视图，供渲染与 JSON 使用。
func (s *State) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	samples := make([]Sample, len(s.samples))
	copy(samples, s.samples)
	return Snapshot{
		Samples:     samples,
		Capacity:    s.capacity,
		LastAttempt: s.lastAttempt,
		LastSuccess: s.lastSuccess,
		LastError:   s.lastError,
		Reachable:   s.reachable,
		SchemaWarn:  s.schemaWarn,
		Role:        s.role,
	}
}

// Snapshot 是 State 在某一时刻的不可变视图。
type Snapshot struct {
	Samples     []Sample
	Capacity    int
	LastAttempt time.Time
	LastSuccess time.Time
	LastError   string
	Reachable   bool
	SchemaWarn  string
	Role        string
}

// Latest 返回最近一条样本。
func (s Snapshot) Latest() (Sample, bool) {
	if len(s.Samples) == 0 {
		return Sample{}, false
	}
	return s.Samples[len(s.Samples)-1], true
}

// HasData 报告是否至少有过一次成功拉取。
func (s Snapshot) HasData() bool { return len(s.Samples) > 0 }

// Stale 返回距最近一次成功拉取的时长；从未成功时 ok=false。
func (s Snapshot) Stale(now time.Time) (time.Duration, bool) {
	if s.LastSuccess.IsZero() {
		return 0, false
	}
	return now.Sub(s.LastSuccess), true
}

// Derived 是面板在页面上展示的派生指标；无法计算（除零、无数据）时为 nil。
type Derived struct {
	// WirePayloadRatio 是累计线字节 / 累计载荷字节，越接近 1 链路开销越小。
	WirePayloadRatio *float64 `json:"wire_payload_ratio"`
	// AvgPayloadBps 是进程启动至今的平均载荷速率（上下行合计）。
	AvgPayloadBps *float64 `json:"avg_payload_bps"`
	// StreamsPerSession 是累计流数 / 累计会话数（当前每会话单流，理想接近 1）。
	StreamsPerSession *float64 `json:"streams_per_session"`
	// ErrorsPer1kStreams 是累计错误数换算成「每千条流」的错误数。
	ErrorsPer1kStreams *float64 `json:"errors_per_1k_streams"`
	// FECSuccessRatio 是窗口内 fec_recovered/(fec_recovered+fec_errs)。
	FECSuccessRatio *float64 `json:"fec_success_ratio"`
	// StaleSeconds 是页面生成时刻距最近一次成功拉取的秒数。
	StaleSeconds *float64 `json:"stale_seconds"`
}

// Derived 计算派生指标。
func (s Snapshot) Derived(now time.Time) Derived {
	smp, ok := s.Latest()
	if !ok {
		return Derived{}
	}
	m := smp.Metrics
	var d Derived
	if m.Payload.SentTotal+m.Payload.RecvTotal > 0 {
		d.WirePayloadRatio = ratio(float64(m.Wire.SentTotal+m.Wire.RecvTotal), float64(m.Payload.SentTotal+m.Payload.RecvTotal))
	}
	if m.UptimeSeconds > 0 {
		d.AvgPayloadBps = ratio(float64(m.Payload.SentTotal+m.Payload.RecvTotal), m.UptimeSeconds)
	}
	if m.Sessions.Total > 0 {
		d.StreamsPerSession = ratio(float64(m.Streams.Total), float64(m.Sessions.Total))
	}
	if m.Streams.Total > 0 {
		d.ErrorsPer1kStreams = ratio(float64(TotalErrors(m)), float64(m.Streams.Total)/1000)
	}
	fecOK, fecErr := windowUint(m.Link.Window.FECRecovered), windowUint(m.Link.Window.FECErrs)
	if fecOK+fecErr > 0 {
		d.FECSuccessRatio = ratio(float64(fecOK), float64(fecOK+fecErr))
	}
	if stale, ok := s.Stale(now); ok {
		d.StaleSeconds = ptr(stale.Seconds())
	}
	return d
}

// ratio 计算 a/b；b 为 0 时返回 nil，避免页面出现 Inf/NaN。
func ratio(a, b float64) *float64 {
	if b == 0 {
		return nil
	}
	return ptr(a / b)
}

func ptr(v float64) *float64 { return &v }

func windowUint(v *uint64) uint64 {
	if v == nil {
		return 0
	}
	return *v
}

// counterSub 计算无符号增量；计数器回退（例如上游重启）时返回 0 而不是负数。
func counterSub(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}
