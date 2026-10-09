package metrics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HTTPOptions 描述统计端点的对外口径。
type HTTPOptions struct {
	// Role 是进程角色（client / server）。
	Role string
	// StartedAt 是进程启动时刻。
	StartedAt time.Time
	// DefaultWindow 是 /metrics 不带 window 参数时的速率窗口（= metrics_interval）。
	DefaultWindow time.Duration
	// Now 便于测试注入时间；为 nil 时用 time.Now。
	Now func() time.Time
}

// NewHTTPHandler 构造统计端点：
//
//	GET /metrics?window=10s   结构化统计（JSON）
//	GET /healthz              存活探测（JSON）
//
// window 需在 [Resolution, Retention] 之间；缺省用 DefaultWindow。
// 其他路径 404，非 GET/HEAD 405。累计计数器是请求时刻的新鲜值，
// 速率与增量是该窗口两端算出的值（历史不足时 rate_available=false 且相关字段为 null）。
func NewHTTPHandler(c *Collector, o HTTPOptions) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.DefaultWindow <= 0 {
		o.DefaultWindow = Resolution
	}
	h := &statsHandler{collector: c, opts: o}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", h.metrics)
	mux.HandleFunc("/healthz", h.healthz)
	mux.HandleFunc("/", h.notFound)
	return mux
}

type statsHandler struct {
	collector *Collector
	opts      HTTPOptions
}

func (h *statsHandler) metrics(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	window, err := h.window(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload{Error: err.Error()})
		return
	}
	now := h.opts.Now()
	writeJSON(w, http.StatusOK, buildMetricsPayload(h.collector.View(window), h.opts, now))
}

func (h *statsHandler) healthz(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	now := h.opts.Now()
	writeJSON(w, http.StatusOK, healthPayload{
		Status:        "ok",
		Role:          h.opts.Role,
		StartedAt:     h.opts.StartedAt,
		UptimeSeconds: seconds(now.Sub(h.opts.StartedAt)),
		Now:           now,
	})
}

func (h *statsHandler) notFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, errorPayload{Error: fmt.Sprintf("未知路径 %s（可用：/metrics、/healthz）", r.URL.Path)})
}

// window 解析并校验 ?window=。
func (h *statsHandler) window(r *http.Request) (time.Duration, error) {
	raw := r.URL.Query().Get("window")
	if raw == "" {
		return clampWindow(h.opts.DefaultWindow), nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("window 需要 duration 字符串（如 1s、10s、30s）: %v", err)
	}
	if d < Resolution || d > Retention {
		return 0, fmt.Errorf("window 需在 %s~%s 之间，实际 %s", Resolution, Retention, d)
	}
	return d, nil
}

// 线上格式按 schema_version 固定字段名；速率相关字段用指针以便输出 null。
type metricsPayload struct {
	SchemaVersion     int           `json:"schema_version"`
	Role              string        `json:"role"`
	StartedAt         time.Time     `json:"started_at"`
	UptimeSeconds     float64       `json:"uptime_seconds"`
	Now               time.Time     `json:"now"`
	RateWindowSeconds float64       `json:"rate_window_seconds"`
	RateAvailable     bool          `json:"rate_available"`
	Sessions          countPair     `json:"sessions"`
	Streams           countPair     `json:"streams"`
	Payload           ioRates       `json:"payload"`
	Wire              ioRates       `json:"wire"`
	Link              linkPayload   `json:"link"`
	Pool              poolPayload   `json:"pool"`
	Errors            errorCounters `json:"errors"`
	Alarms            []string      `json:"alarms"`
}

type countPair struct {
	Active int64  `json:"active"`
	Total  uint64 `json:"total"`
}

type ioRates struct {
	SentTotal uint64   `json:"sent_total"`
	RecvTotal uint64   `json:"recv_total"`
	SentBps   *float64 `json:"sent_bps"`
	RecvBps   *float64 `json:"recv_bps"`
}

type linkWindow struct {
	Seconds               float64  `json:"seconds"`
	OutSegs               *uint64  `json:"out_segs"`
	RetransRatio          *float64 `json:"retrans_ratio"`
	RetransRatioAvailable bool     `json:"retrans_ratio_available"`
	LostSegs              *uint64  `json:"lost_segs"`
	RepeatSegs            *uint64  `json:"repeat_segs"`
	KCPInErrors           *uint64  `json:"kcp_in_errors"`
	FECRecovered          *uint64  `json:"fec_recovered"`
	FECErrs               *uint64  `json:"fec_errs"`
}

type linkPayload struct {
	OutSegsTotal         uint64     `json:"out_segs_total"`
	InSegsTotal          uint64     `json:"in_segs_total"`
	RetransSegsTotal     uint64     `json:"retrans_segs_total"`
	FastRetransSegsTotal uint64     `json:"fast_retrans_segs_total"`
	Window               linkWindow `json:"window"`
}

type poolPayload struct {
	Available bool   `json:"available"`
	InUse     int    `json:"in_use"`
	Idle      int    `json:"idle"`
	Creating  int    `json:"creating"`
	Waiters   int    `json:"waiters"`
	Rebuilds  uint64 `json:"rebuilds"`
}

type errorCounters struct {
	Auth    uint64 `json:"auth"`
	Session uint64 `json:"session"`
	Socks5  uint64 `json:"socks5"`
	Dial    uint64 `json:"dial"`
}

type healthPayload struct {
	Status        string    `json:"status"`
	Role          string    `json:"role"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	Now           time.Time `json:"now"`
}

type errorPayload struct {
	Error string `json:"error"`
}

// buildMetricsPayload 把一次 View 转成线上 JSON 结构。
func buildMetricsPayload(v View, o HTTPOptions, now time.Time) metricsPayload {
	r := v.Now.Registry
	win := linkWindow{Seconds: v.UserWindow.Seconds()}
	p := metricsPayload{
		SchemaVersion:     1,
		Role:              o.Role,
		StartedAt:         o.StartedAt,
		UptimeSeconds:     seconds(now.Sub(o.StartedAt)),
		Now:               now,
		RateWindowSeconds: v.UserWindow.Seconds(),
		RateAvailable:     v.HasRates,
		Sessions:          countPair{Active: r.SessionsActive, Total: r.SessionsTotal},
		Streams:           countPair{Active: r.StreamsActive, Total: r.StreamsTotal},
		Payload: ioRates{
			SentTotal: r.PayloadSent,
			RecvTotal: r.PayloadReceived,
		},
		Wire: ioRates{
			SentTotal: v.Now.Transport.UDPBytesSent,
			RecvTotal: v.Now.Transport.UDPBytesReceived,
		},
		Link: linkPayload{
			OutSegsTotal:         v.Now.Transport.OutSegs,
			InSegsTotal:          v.Now.Transport.InSegs,
			RetransSegsTotal:     v.Now.Transport.RetransSegs,
			FastRetransSegsTotal: v.Now.Transport.FastRetransSegs,
			Window:               win,
		},
		Pool: poolPayload{
			Available: v.HasPool,
			InUse:     v.Now.Pool.InUse,
			Idle:      v.Now.Pool.Idle,
			Creating:  v.Now.Pool.Creating,
			Waiters:   v.Now.Pool.Waiters,
			Rebuilds:  v.Now.Pool.Rebuilds,
		},
		Errors: errorCounters{
			Auth:    r.AuthFailures,
			Session: r.SessionDialFailures,
			Socks5:  r.Socks5Failures,
			Dial:    r.DialFailures,
		},
		Alarms: []string{},
	}
	if v.HasRates {
		smp := v.Sample
		p.Payload.SentBps = &smp.PayloadSentBps
		p.Payload.RecvBps = &smp.PayloadRecvBps
		p.Wire.SentBps = &smp.WireSentBps
		p.Wire.RecvBps = &smp.WireRecvBps
		p.Link.Window.Seconds = smp.Interval.Seconds()
		p.Link.Window.OutSegs = &smp.OutSegsDelta
		p.Link.Window.LostSegs = &smp.LostSegsDelta
		p.Link.Window.RepeatSegs = &smp.RepeatSegsDelta
		p.Link.Window.KCPInErrors = &smp.KCPInErrorsDelta
		p.Link.Window.FECRecovered = &smp.FECRecoveredDelta
		p.Link.Window.FECErrs = &smp.FECErrsDelta
		p.Link.Window.RetransRatioAvailable = smp.OutSegsDelta >= minRetransSampleSegs
		if p.Link.Window.RetransRatioAvailable {
			p.Link.Window.RetransRatio = &smp.RetransRate
		}
	}
	if alarms := Alarms(v); len(alarms) > 0 {
		p.Alarms = alarms
	}
	return p
}

// allowGet 只接受 GET/HEAD；否则回 405。
func allowGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeJSON(w, http.StatusMethodNotAllowed, errorPayload{Error: fmt.Sprintf("不支持的方法 %s（只支持 GET/HEAD）", r.Method)})
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// seconds 把时长转成秒（保留小数）。
func seconds(d time.Duration) float64 { return d.Seconds() }
