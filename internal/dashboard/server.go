package dashboard

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/LiZeC123/gks/internal/metrics"
)

// stateSchemaVersion 是 /api/state 的格式版本（与上游 /metrics 的 schema_version 无关）。
const stateSchemaVersion = 1

// assets 是内嵌的页面资源：index.html（模板）、style.css、app.js。
// 单文件分发靠它——运行时不读任何外部文件。
//
//go:embed assets
var assetsFS embed.FS

// ServerOptions 是面板 HTTP 服务的设置。
type ServerOptions struct {
	// State 是共享状态。
	State *State
	// Page 是展示层的静态参数。
	Page PageOptions
	// Now 便于测试注入时间；为 nil 时用 time.Now。
	Now func() time.Time
}

// Server 渲染面板页面并提供 JSON 接口。
type Server struct {
	state  *State
	page   PageOptions
	tmpl   *template.Template
	static http.Handler
	now    func() time.Time
}

// NewServer 解析内嵌模板并构造服务。
func NewServer(o ServerOptions) (*Server, error) {
	if o.State == nil {
		return nil, errors.New("dashboard: Server 需要 State")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	tmpl, err := template.New("index.html").Funcs(templateFuncs()).ParseFS(assetsFS, "assets/*.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: 解析内嵌模板: %w", err)
	}
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return nil, fmt.Errorf("dashboard: 内嵌静态资源: %w", err)
	}
	return &Server{
		state:  o.State,
		page:   o.Page,
		tmpl:   tmpl,
		static: http.FileServer(http.FS(sub)),
		now:    o.Now,
	}, nil
}

// Handler 返回面板的全部路由：
//
//	GET /            完整页面（首屏服务端渲染，含 SVG 折线图）
//	GET /partial     只返回内容片段，供页面内 JS 局部刷新
//	GET /api/state   面板状态 + 上游最近 payload + 历史序列（JSON，供脚本消费）
//	GET /healthz     面板存活（body 里带上游可达状态）
//	GET /static/*    内嵌的 css/js
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/partial", s.handlePartial)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.Handle("/static/", http.StripPrefix("/static/", s.static))
	return mux
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, errPayload{Error: fmt.Sprintf("未知路径 %s（可用：/、/partial、/api/state、/healthz）", r.URL.Path)})
		return
	}
	s.render(w, "page")
}

func (s *Server) handlePartial(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	s.render(w, "content")
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.stateDTO())
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	snap := s.state.Snapshot()
	now := s.now()
	body := map[string]any{
		"status":   "ok",
		"upstream": snap.Reachable,
		"role":     snap.Role,
		"now":      now,
	}
	if stale, ok := snap.Stale(now); ok {
		body["stale_seconds"] = stale.Seconds()
		body["last_success"] = snap.LastSuccess
	}
	if snap.LastError != "" {
		body["last_error"] = snap.LastError
	}
	writeJSON(w, http.StatusOK, body)
}

// render 先在内存里渲染完整，避免模板出错时输出半截页面。
func (s *Server) render(w http.ResponseWriter, name string) {
	page := NewPage(s.state.Snapshot(), s.page, s.now())
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, page); err != nil {
		http.Error(w, "面板模板渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// stateDTO 是 /api/state 的响应体。
type stateDTO struct {
	SchemaVersion int         `json:"schema_version"`
	Dashboard     dashInfo    `json:"dashboard"`
	Upstream      upstreamDTO `json:"upstream"`
	Latest        *latestDTO  `json:"latest"`
	Derived       Derived     `json:"derived"`
	Alarms        []string    `json:"alarms"`
	History       []pointDTO  `json:"history"`
}

type dashInfo struct {
	PullURL         string    `json:"pull_url"`
	Listen          string    `json:"listen"`
	IntervalSeconds float64   `json:"interval_seconds"`
	RefreshSeconds  float64   `json:"refresh_seconds"`
	SampleCapacity  int       `json:"sample_capacity"`
	Samples         int       `json:"samples"`
	Now             time.Time `json:"now"`
}

type upstreamDTO struct {
	Reachable     bool       `json:"reachable"`
	Role          string     `json:"role,omitempty"`
	LastAttempt   *time.Time `json:"last_attempt,omitempty"`
	LastSuccess   *time.Time `json:"last_success,omitempty"`
	StaleSeconds  *float64   `json:"stale_seconds"`
	LastError     string     `json:"last_error,omitempty"`
	SchemaWarning string     `json:"schema_warning,omitempty"`
}

// latestDTO 是上游最近一次 /metrics 的关键字段（面板自己算的派生量另见 derived）。
type latestDTO struct {
	SchemaVersion  int                   `json:"schema_version"`
	Role           string                `json:"role"`
	StartedAt      time.Time             `json:"started_at"`
	UptimeSeconds  float64               `json:"uptime_seconds"`
	RateWindowSecs float64               `json:"rate_window_seconds"`
	RateAvailable  bool                  `json:"rate_available"`
	Sessions       metrics.CountPair     `json:"sessions"`
	Streams        metrics.CountPair     `json:"streams"`
	Payload        metrics.IORates       `json:"payload"`
	Wire           metrics.IORates       `json:"wire"`
	Link           metrics.LinkPayload   `json:"link"`
	Pool           metrics.PoolPayload   `json:"pool"`
	Errors         metrics.ErrorCounters `json:"errors"`
	UpstreamAlarms []string              `json:"upstream_alarms"`
	TotalErrors    uint64                `json:"total_errors"`
	At             time.Time             `json:"at"`
	ReceivedAt     time.Time             `json:"received_at"`
	ErrorsDelta    uint64                `json:"errors_delta"`
	StreamsDelta   uint64                `json:"streams_delta"`
}

type pointDTO struct {
	At             time.Time `json:"at"`
	PayloadSentBps *float64  `json:"payload_sent_bps"`
	PayloadRecvBps *float64  `json:"payload_recv_bps"`
	WireSentBps    *float64  `json:"wire_sent_bps"`
	WireRecvBps    *float64  `json:"wire_recv_bps"`
	RetransRatio   *float64  `json:"retrans_ratio"`
	ErrorsPer1k    *float64  `json:"errors_per_1k_streams"`
}

type errPayload struct {
	Error string `json:"error"`
}

func (s *Server) stateDTO() stateDTO {
	snap := s.state.Snapshot()
	now := s.now()
	dto := stateDTO{
		SchemaVersion: stateSchemaVersion,
		Dashboard: dashInfo{
			PullURL:         s.page.PullURL,
			Listen:          s.page.Listen,
			IntervalSeconds: s.page.Interval.Seconds(),
			RefreshSeconds:  s.page.Refresh.Seconds(),
			SampleCapacity:  snap.Capacity,
			Samples:         len(snap.Samples),
			Now:             now,
		},
		Upstream: upstreamDTO{
			Reachable:     snap.Reachable,
			Role:          snap.Role,
			LastError:     snap.LastError,
			SchemaWarning: snap.SchemaWarn,
		},
		Derived: snap.Derived(now),
		// 与 gks /metrics 的 alarms 一致：无告警时输出空数组而不是 null。
		Alarms:  append([]string{}, append(panelAlarms(snap, s.page, now), upstreamAlarms(snap)...)...),
		History: []pointDTO{},
	}
	if !snap.LastAttempt.IsZero() {
		at := snap.LastAttempt
		dto.Upstream.LastAttempt = &at
	}
	if !snap.LastSuccess.IsZero() {
		at := snap.LastSuccess
		dto.Upstream.LastSuccess = &at
	}
	if stale, ok := snap.Stale(now); ok {
		v := stale.Seconds()
		dto.Upstream.StaleSeconds = &v
	}
	for _, smp := range snap.Samples {
		dto.History = append(dto.History, pointDTO{
			At:             smp.Received,
			PayloadSentBps: smp.Metrics.Payload.SentBps,
			PayloadRecvBps: smp.Metrics.Payload.RecvBps,
			WireSentBps:    smp.Metrics.Wire.SentBps,
			WireRecvBps:    smp.Metrics.Wire.RecvBps,
			RetransRatio:   smp.RetransRatio(),
			ErrorsPer1k:    smp.ErrorsPer1k(),
		})
	}
	if latest, ok := snap.Latest(); ok {
		m := latest.Metrics
		dto.Latest = &latestDTO{
			SchemaVersion:  m.SchemaVersion,
			Role:           m.Role,
			StartedAt:      m.StartedAt,
			UptimeSeconds:  m.UptimeSeconds,
			RateWindowSecs: m.RateWindowSeconds,
			RateAvailable:  m.RateAvailable,
			Sessions:       m.Sessions,
			Streams:        m.Streams,
			Payload:        m.Payload,
			Wire:           m.Wire,
			Link:           m.Link,
			Pool:           m.Pool,
			Errors:         m.Errors,
			UpstreamAlarms: m.Alarms,
			TotalErrors:    TotalErrors(m),
			At:             latest.At,
			ReceivedAt:     latest.Received,
			ErrorsDelta:    latest.ErrorsDelta,
			StreamsDelta:   latest.StreamsDelta,
		}
	}
	return dto
}

// allowGet 只接受 GET/HEAD；否则回 405。
func allowGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeJSON(w, http.StatusMethodNotAllowed, errPayload{Error: fmt.Sprintf("不支持的方法 %s（只支持 GET/HEAD）", r.Method)})
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
