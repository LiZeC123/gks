// Command dashboard 是 gks 的独立监控面板。
//
// 形态：周期拉取某个 gks 实例的统计端点（默认 127.0.0.1:12081），另起一个 HTTP 服务
// （默认 0.0.0.0:12080），浏览器打开即可看到累计数据、窗口速率、四张折线图与派生指标。
//
// 与 gks 完全解耦：不读它的配置、不共享进程，拉不到数据时继续重试并在页面上标红；
// 页面与静态资源全部内嵌进二进制，单文件即可分发。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LiZeC123/gks/internal/dashboard"
	"github.com/LiZeC123/gks/internal/log"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gks-dashboard: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen   string
		pull     string
		interval time.Duration
		window   time.Duration
		timeout  time.Duration
		refresh  time.Duration
		history  int
		logLevel string
	)
	flag.StringVar(&listen, "listen", "0.0.0.0:12080", "面板 HTTP 监听地址（TCP，JSON 与页面）")
	flag.StringVar(&pull, "pull", "127.0.0.1:12081", "gks 统计端点地址，host:port 或完整 URL")
	flag.DurationVar(&interval, "interval", time.Second, "拉取间隔")
	flag.DurationVar(&window, "window", 0, "传给上游的速率窗口 ?window=（默认 = interval）")
	flag.DurationVar(&timeout, "timeout", 3*time.Second, "单次拉取超时")
	flag.DurationVar(&refresh, "refresh", 2*time.Second, "页面局部刷新间隔（0 = 只渲染首屏）")
	flag.IntVar(&history, "history", 300, "保留的采样点数（趋势图最多画这么多点）")
	flag.StringVar(&logLevel, "log-level", "info", "日志级别 debug|info|warn|error")
	flag.Parse()

	if history < 2 {
		return fmt.Errorf("-history 必须不小于 2（错误率曲线需要相邻两个样本），实际 %d", history)
	}
	if refresh < 0 {
		return errors.New("-refresh 不能为负（0 表示只渲染首屏）")
	}
	metricsURL, err := dashboard.NormalizeMetricsURL(pull)
	if err != nil {
		return err
	}
	logger, err := log.New(os.Stderr, logLevel)
	if err != nil {
		return err
	}

	// 先绑监听：端口被占属于配置错误，直接失败退出（与 gks 的监听语义一致）。
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", listen, err)
	}
	defer func() { _ = ln.Close() }()

	state := dashboard.NewState(history)
	poller, err := dashboard.NewPoller(dashboard.PollerOptions{
		URL:      metricsURL,
		Interval: interval,
		Window:   window,
		Timeout:  timeout,
		State:    state,
		Logger:   logger,
	})
	if err != nil {
		return err
	}
	srv, err := dashboard.NewServer(dashboard.ServerOptions{
		State: state,
		Page: dashboard.PageOptions{
			PullURL:  metricsURL,
			Listen:   ln.Addr().String(),
			Interval: interval,
			Refresh:  refresh,
			Capacity: history,
		},
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go poller.Run(ctx)

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Info("gks 监控面板已启动",
		log.Event, "dashboard_start",
		"listen", ln.Addr().String(),
		"pull", metricsURL,
		"interval", interval.String(),
		"window", poller.Window().String(),
		"history", history,
		"refresh", refresh.String(),
	)
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("gks 监控面板已退出", log.Event, "dashboard_stop")
	return nil
}
