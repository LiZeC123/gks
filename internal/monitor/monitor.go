// Package monitor 把观测组件接成一个可启停的整体：
// 1s 粒度的速率采样、控制台表格周期刷新、统计 HTTP 端点。
//
// 启动顺序有意做成「先绑端点、再继续启动业务」：端点绑定失败直接返回错误，
// 由调用方终止启动（fail-fast），避免监控静默失效而无人察觉。
package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/LiZeC123/gks/internal/log"
	"github.com/LiZeC123/gks/internal/metrics"
)

// Options 描述观测组件的启动参数。
type Options struct {
	// Role 是进程角色（client / server）。
	Role string
	// Collector 是共享的统计采集器。
	Collector *metrics.Collector
	// HTTPAddr 是统计端点地址；为空表示不启动端点。
	HTTPAddr string
	// Console 为 true 时周期刷新控制台表格。
	Console bool
	// ConsoleOut 是表格输出目标（通常 os.Stdout）；为 nil 时用 os.Stdout。
	ConsoleOut io.Writer
	// StartedAt 是进程启动时刻。
	StartedAt time.Time
	// Logger 接收内部错误；为 nil 时用 slog.Default()。
	Logger *slog.Logger
}

// Monitor 是已启动的观测组件。
type Monitor struct {
	opts Options
	out  io.Writer
	tty  bool

	ln     net.Listener
	srv    *http.Server
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu    sync.Mutex
	drawn int // TTY 下已画出的行数，用于原地刷新
}

// Start 启动观测组件。HTTPAddr 非空时先绑定监听，失败立即返回错误。
func Start(parent context.Context, o Options) (*Monitor, error) {
	if o.Collector == nil {
		return nil, errors.New("monitor: Collector 不能为空")
	}
	if o.ConsoleOut == nil {
		o.ConsoleOut = os.Stdout
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	m := &Monitor{opts: o, out: o.ConsoleOut, tty: IsTerminal(o.ConsoleOut)}

	if o.HTTPAddr != "" {
		ln, err := net.Listen("tcp", o.HTTPAddr)
		if err != nil {
			return nil, fmt.Errorf("统计端点监听 %s: %w", o.HTTPAddr, err)
		}
		m.ln = ln
		m.srv = &http.Server{
			Handler: metrics.NewHTTPHandler(o.Collector, metrics.HTTPOptions{
				Role:          o.Role,
				StartedAt:     o.StartedAt,
				DefaultWindow: o.Collector.Interval(),
			}),
			ReadHeaderTimeout: 5 * time.Second,
		}
	}

	ctx, cancel := context.WithCancel(parent)
	m.cancel = cancel

	// 只有「端点启用或表格启用」才需要 1s 采样；两者都关时完全不采样。
	if o.HTTPAddr != "" || o.Console {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			o.Collector.Run(ctx)
		}()
	}
	if o.Console {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.consoleLoop(ctx)
		}()
	}
	if m.srv != nil {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.serve()
		}()
	}
	return m, nil
}

// Addr 返回统计端点的实际监听地址（未启用时为空串）。
func (m *Monitor) Addr() string {
	if m.ln == nil {
		return ""
	}
	return m.ln.Addr().String()
}

// TTY 报告控制台表格是否按「原地刷新」方式输出。
func (m *Monitor) TTY() bool { return m.tty }

// Close 停止观测组件；控制台表格开启时会再渲染一次终态。
func (m *Monitor) Close() {
	if m.cancel != nil {
		m.cancel()
	}
	if m.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = m.srv.Shutdown(ctx)
		cancel()
	} else if m.ln != nil {
		_ = m.ln.Close()
	}
	m.wg.Wait()
	if m.opts.Console {
		m.render()
	}
}

func (m *Monitor) serve() {
	if err := m.srv.Serve(m.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		m.opts.Logger.Error("统计端点退出", log.Event, "metrics_http_error", "err", err)
	}
}

// consoleLoop 周期性渲染表格：先立刻渲染一次（此时速率列是 n/a），之后按
// metrics_interval 刷新。
func (m *Monitor) consoleLoop(ctx context.Context) {
	period := m.opts.Collector.Interval()
	if period < metrics.Resolution {
		period = metrics.Resolution
	}
	m.render()
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.render()
		}
	}
}

func (m *Monitor) render() {
	v := m.opts.Collector.View(m.opts.Collector.Interval())
	text := metrics.RenderTable(v, metrics.TableOptions{
		Role:      m.opts.Role,
		StartedAt: m.opts.StartedAt,
	})
	m.draw(text)
}

// draw 输出表格：TTY 用「光标上移 + 清行」原地刷新，非 TTY（重定向/管道）
// 每个周期追加一份纯文本表格（不写任何 ANSI 转义）。
func (m *Monitor) draw(text string) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.tty {
		fmt.Fprint(m.out, text, "\n")
		return
	}
	if m.drawn == 0 {
		// 首次渲染：光标本来就在行首，直接画即可。
		for _, ln := range lines {
			fmt.Fprintln(m.out, ln)
		}
		m.drawn = len(lines)
		return
	}
	fmt.Fprintf(m.out, "\x1b[%dA", m.drawn)
	for _, ln := range lines {
		fmt.Fprintf(m.out, "\r\x1b[2K%s\n", ln)
	}
	// 本次行数少于上次（例如「历史不足」提示行消失）：清掉多余行再回到表格末尾。
	for i := len(lines); i < m.drawn; i++ {
		fmt.Fprint(m.out, "\r\x1b[2K\n")
	}
	if len(lines) < m.drawn {
		fmt.Fprintf(m.out, "\x1b[%dA", m.drawn-len(lines))
	}
	m.drawn = len(lines)
}

// IsTerminal 报告 w 是否指向字符设备（终端）。重定向到文件或管道时为 false。
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
