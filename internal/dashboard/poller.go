package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LiZeC123/gks/internal/log"
	"github.com/LiZeC123/gks/internal/metrics"
)

// maxMetricsBody 是单次拉取的响应体上限（防止异常上游把面板内存打满）。
const maxMetricsBody = 4 << 20

// PollerOptions 是拉取器的设置。
type PollerOptions struct {
	// URL 是已归一化的 /metrics 地址，例如 http://127.0.0.1:12081/metrics。
	URL string
	// Interval 是拉取间隔。
	Interval time.Duration
	// Window 是传给上游的速率窗口（?window=）；为 0 时用 Interval。
	Window time.Duration
	// Timeout 是单次拉取超时。
	Timeout time.Duration
	// State 接收拉取结果。
	State *State
	// Logger 记录状态迁移（上游恢复/断开）；为 nil 时用 slog.Default()。
	Logger *slog.Logger
	// Client 便于测试注入；为 nil 时按 Timeout 构造。
	Client *http.Client
}

// Poller 周期拉取上游 /metrics 并写进 State。
type Poller struct {
	opts   PollerOptions
	client *http.Client
}

// NewPoller 校验参数并构造拉取器。
func NewPoller(o PollerOptions) (*Poller, error) {
	if o.State == nil {
		return nil, errors.New("dashboard: Poller 需要 State")
	}
	if o.URL == "" {
		return nil, errors.New("dashboard: Poller 需要拉取地址")
	}
	if o.Interval <= 0 {
		return nil, fmt.Errorf("dashboard: 拉取间隔必须大于 0，实际 %s", o.Interval)
	}
	if o.Timeout <= 0 {
		return nil, fmt.Errorf("dashboard: 拉取超时必须大于 0，实际 %s", o.Timeout)
	}
	if o.Window <= 0 {
		o.Window = o.Interval
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: o.Timeout}
	}
	return &Poller{opts: o, client: client}, nil
}

// Interval 返回拉取间隔。
func (p *Poller) Interval() time.Duration { return p.opts.Interval }

// Window 返回传给上游的速率窗口。
func (p *Poller) Window() time.Duration { return p.opts.Window }

// Run 先立刻拉一次，然后每 Interval 拉一次，直到 ctx 结束。
//
// 上游不可达只更新状态、不退出：面板必须能与 gks 的启停完全解耦。
func (p *Poller) Run(ctx context.Context) {
	p.pollAndLog(ctx)
	ticker := time.NewTicker(p.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollAndLog(ctx)
		}
	}
}

func (p *Poller) pollAndLog(ctx context.Context) {
	wasReachable := p.opts.State.Snapshot().Reachable
	err := p.PollOnce(ctx)
	if err != nil {
		if wasReachable {
			p.opts.Logger.Warn("上游不可达，保留已有数据继续重试",
				log.Event, "upstream_down",
				"url", p.opts.URL,
				"err", err,
			)
		}
		return
	}
	if !wasReachable {
		p.opts.Logger.Info("上游已恢复", log.Event, "upstream_up", "url", p.opts.URL)
	}
}

// PollOnce 拉取并解析一次；失败时把错误写进 State 并返回该错误。
func (p *Poller) PollOnce(ctx context.Context) error {
	now := time.Now()
	payload, err := p.fetch(ctx)
	if err != nil {
		p.opts.State.MarkFailure(now, err.Error())
		return err
	}
	warn := ""
	if payload.SchemaVersion != metrics.SchemaVersion {
		warn = fmt.Sprintf("上游 schema_version=%d，面板按 v%d 解析；请同步升级面板或 gks",
			payload.SchemaVersion, metrics.SchemaVersion)
	}
	p.opts.State.MarkSuccess(Sample{
		At:       payload.Now,
		Received: time.Now(),
		Metrics:  *payload,
	}, warn)
	return nil
}

func (p *Poller) fetch(ctx context.Context) (*metrics.Payload, error) {
	u, err := url.Parse(p.opts.URL)
	if err != nil {
		return nil, fmt.Errorf("拉取地址不合法: %w", err)
	}
	q := u.Query()
	q.Set("window", p.opts.Window.String())
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("上游返回 %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var payload metrics.Payload
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxMetricsBody))
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析上游 JSON: %w", err)
	}
	return &payload, nil
}

// NormalizeMetricsURL 把用户给的拉取地址归一化成 /metrics 的完整 URL：
//
//	127.0.0.1:12081        → http://127.0.0.1:12081/metrics
//	http://host:12081      → http://host:12081/metrics
//	http://host:12081/m    → 原样保留路径（面板会用 ?window= 覆盖查询参数）
//
// 必须带端口：这个工具就是拉本机 gks 的统计端口，漏写端口几乎都是笔误。
func NormalizeMetricsURL(pull string) (string, error) {
	raw := strings.TrimSpace(pull)
	if raw == "" {
		return "", errors.New("拉取地址不能为空")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("拉取地址不合法 %q: %w", pull, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("拉取地址只支持 http/https，实际 %q", u.Scheme)
	}
	if u.Hostname() == "" || u.Port() == "" {
		return "", fmt.Errorf("拉取地址需要 host:port（例如 127.0.0.1:12081），实际 %q", pull)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/metrics"
	}
	return u.String(), nil
}
