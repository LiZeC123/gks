package metrics

import (
	"context"
	"sync"
	"time"
)

// 速率历史的时间参数。Resolution 同时也是端点 ?window= 的下限，Retention 是上限。
const (
	// Resolution 是速率历史的采样粒度（1s）。
	Resolution = time.Second
	// Retention 是速率历史的保留时长（10 分钟）。
	Retention = 10 * time.Minute
)

// Options 描述采集器的数据来源与刷新周期。
type Options struct {
	// Registry 是进程级计数器；为 nil 时使用 Default。
	Registry *Registry
	// Sources 提供传输层与会话池数据；Pool 为 nil 表示本进程没有会话池（服务端）。
	Sources Sources
	// Interval 是控制台表格刷新周期，同时也是端点默认速率窗口。
	// 配置校验保证它大于 0；这里对 <= 0 的取值不做处理（View 会把窗口夹到合法区间）。
	Interval time.Duration
}

// Collector 维护累计计数器与 1s 粒度的环形速率历史。
//
// 它只负责「取数」与「算数」：不写日志、不渲染、不监听端口，
// 因此控制台表格与 HTTP 端点可以共用同一份历史而互不干扰。
type Collector struct {
	reg       *Registry
	transport func() TransportStats
	pool      func() PoolStats
	hasPool   bool
	interval  time.Duration

	mu      sync.Mutex
	history []Snapshot // 按时间升序，最多 Retention/Resolution 条
}

// NewCollector 构造采集器。
func NewCollector(o Options) *Collector {
	reg := o.Registry
	if reg == nil {
		reg = Default
	}
	transport := o.Sources.Transport
	if transport == nil {
		transport = func() TransportStats { return TransportStats{} }
	}
	pool := o.Sources.Pool
	hasPool := pool != nil
	if pool == nil {
		pool = func() PoolStats { return PoolStats{} }
	}
	return &Collector{
		reg:       reg,
		transport: transport,
		pool:      pool,
		hasPool:   hasPool,
		interval:  o.Interval,
	}
}

// Interval 返回控制台刷新周期（同时是端点默认速率窗口）。
func (c *Collector) Interval() time.Duration { return c.interval }

// PoolAvailable 报告本进程是否会话池统计（服务端为 false）。
func (c *Collector) PoolAvailable() bool { return c.hasPool }

// Snapshot 采集当前时刻的一条新鲜快照（累计计数器 + 瞬时值 + 池状态）。
func (c *Collector) Snapshot() Snapshot { return c.snapshotAt(time.Now()) }

func (c *Collector) snapshotAt(now time.Time) Snapshot {
	return c.reg.Take(c.transport(), c.pool(), now)
}

// Record 把一条快照写入历史（主要给测试与自定义采样节奏使用）。
func (c *Collector) Record(s Snapshot) { c.record(s) }

// Run 阻塞直到 ctx 结束，按 Resolution 粒度把快照写入历史。
//
// 只有「端点启用或控制台表格启用」时才需要调用它；两者都关闭时完全不需要采样。
func (c *Collector) Run(ctx context.Context) {
	ticker := time.NewTicker(Resolution)
	defer ticker.Stop()
	// 先记一条基准，这样第一秒内就有窗口起点可算（虽然窗口会偏短）。
	c.record(c.Snapshot())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.record(c.Snapshot())
		}
	}
}

func (c *Collector) record(s Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.history = append(c.history, s)
	// 丢弃超出保留时长的旧样本；用 copy 原地压缩，避免底层数组无限增长。
	cut := 0
	for cut < len(c.history) && s.At.Sub(c.history[cut].At) > Retention {
		cut++
	}
	if cut > 0 {
		n := copy(c.history, c.history[cut:])
		c.history = c.history[:n]
	}
}

// Latest 返回最近一条历史样本。
func (c *Collector) Latest() (Snapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.history) == 0 {
		return Snapshot{}, false
	}
	return c.history[len(c.history)-1], true
}

// HistoryLen 返回当前历史样本条数（测试与自检用）。
func (c *Collector) HistoryLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.history)
}

// lookup 找到「时刻不晚于 at-window」的最近一条历史样本，作为窗口起点。
func (c *Collector) lookup(at time.Time, window time.Duration) (Snapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	target := at.Add(-window)
	for i := len(c.history) - 1; i >= 0; i-- {
		if !c.history[i].At.After(target) {
			return c.history[i], true
		}
	}
	return Snapshot{}, false
}

// View 是一次统计呈现的结果：新鲜的点值/累计量 + 指定窗口内的速率与增量。
type View struct {
	// Now 是呈现时刻的新鲜快照：活跃会话/流、累计字节、错误计数、池状态。
	Now Snapshot
	// Sample 是窗口内的速率与增量；HasRates 为 false 时速率与增量都是 0。
	Sample Sample
	// HasRates 表示历史是否已积累够一个完整窗口（启动初期为 false）。
	HasRates bool
	// UserWindow 是调用方请求的窗口（可能被夹到 [Resolution, Retention]）。
	UserWindow time.Duration
	// Window 是实际窗口长度（= Sample.Interval）；HasRates 为 false 时无意义。
	Window time.Duration
	// HasPool 表示本进程有会话池统计（服务端为 false）。
	HasPool bool
}

// View 取一条新鲜快照，并以它作为窗口终点、从历史里找窗口起点算出速率。
//
// 窗口被夹到 [Resolution, Retention]；历史不足（启动初期或窗口超过已积累时长）时
// HasRates 为 false，调用方应把速率字段渲染成 n/a。
func (c *Collector) View(window time.Duration) View {
	window = clampWindow(window)
	now := c.snapshotAt(time.Now())
	v := View{Now: now, Sample: Sample{Current: now}, UserWindow: window, HasPool: c.hasPool}
	prev, ok := c.lookup(now.At, window)
	if !ok {
		return v
	}
	v.Sample = computeSample(prev, now)
	v.HasRates = v.Sample.Interval > 0
	if v.HasRates {
		v.Window = v.Sample.Interval
	}
	return v
}

// clampWindow 把窗口夹到 [Resolution, Retention]。
func clampWindow(w time.Duration) time.Duration {
	switch {
	case w < Resolution:
		return Resolution
	case w > Retention:
		return Retention
	default:
		return w
	}
}
