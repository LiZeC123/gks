// Command client 是 gks 的客户端。
//
// 当前为阶段二形态（dev.md §8）：与 Server 建立 KCP 会话、完成认证握手、
// 发送若干 Session 级 echo 消息并打印回显，随后等待一次心跳 PONG 并打印统计。
// 阶段三起将改为监听本地 SOCKS5 端口并转发 CONNECT。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LiZeC123/gks/internal/config"
	"github.com/LiZeC123/gks/internal/log"
	"github.com/LiZeC123/gks/internal/mux"
	"github.com/LiZeC123/gks/internal/protocol"
	"github.com/LiZeC123/gks/internal/transport"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gks-client: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cfgPath   string
		count     int
		message   string
		echoWait  time.Duration
		hbTimeout time.Duration
	)
	flag.StringVar(&cfgPath, "c", "gks.yaml", "配置文件路径")
	flag.IntVar(&count, "n", 3, "发送的 echo 消息条数")
	flag.StringVar(&message, "msg", "hello gks", "echo 消息前缀")
	flag.DurationVar(&echoWait, "echo-timeout", 5*time.Second, "单条 echo 的等待上限")
	flag.DurationVar(&hbTimeout, "heartbeat-timeout", 10*time.Second, "等待一次心跳 PONG 的上限")
	flag.Parse()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.ValidateClient(); err != nil {
		return fmt.Errorf("配置校验失败:\n%w", err)
	}

	logger, err := log.New(os.Stderr, cfg.Client.Log.Level)
	if err != nil {
		return err
	}
	psk, err := cfg.Common.PSKBytes()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dialer := transport.NewDialer(transport.Options{
		Interval:     cfg.Common.KCP.Interval.D(),
		MTU:          cfg.Common.KCP.MTU,
		SndWnd:       cfg.Common.KCP.SndWnd,
		RcvWnd:       cfg.Common.KCP.RcvWnd,
		DataShards:   cfg.Common.KCP.DataShards,
		ParityShards: cfg.Common.KCP.ParityShards,
		Crypt:        cfg.Common.CryptName(),
		PSK:          psk,
	})
	conn, err := dialer.Dial(cfg.Client.KCP.Server)
	if err != nil {
		return err
	}

	sess, err := mux.DialSession(ctx, conn, mux.Options{
		PSK:               psk,
		AEAD:              cfg.Common.AEADName(),
		AuthTimeout:       cfg.Client.KCP.AuthTimeout.D(),
		HeartbeatInterval: cfg.Client.KCP.HeartbeatInterval.D(),
		HeartbeatMiss:     cfg.Client.KCP.HeartbeatMiss,
		Logger:            logger,
	})
	if err != nil {
		return fmt.Errorf("建立会话失败: %w", err)
	}
	defer func() { _ = sess.Close() }()

	if err := runEchoes(ctx, sess, count, message, echoWait); err != nil {
		return err
	}
	waitHeartbeat(sess, cfg.Client.KCP.HeartbeatInterval.D(), hbTimeout)

	st := sess.Stats()
	fmt.Printf("会话统计: conv=%d role=%s secure=%v frames_in=%d frames_out=%d pings_sent=%d pongs_recv=%d\n",
		st.Conv, st.Role, st.Secure, st.FramesIn, st.FramesOut, st.PingsSent, st.PongsRecv)
	if st.PingsSent > 0 && st.PongsRecv == 0 {
		return errors.New("心跳未收到 PONG")
	}
	return nil
}

func runEchoes(ctx context.Context, sess *mux.Session, count int, prefix string, wait time.Duration) error {
	echoCh := make(chan string, count)
	sess.Handle(protocol.TypeTestEcho, func(_ *mux.Session, f protocol.Frame) {
		select {
		case echoCh <- string(f.Payload):
		default:
		}
	})

	for i := 1; i <= count; i++ {
		msg := fmt.Sprintf("%s #%d", prefix, i)
		if err := sess.SendControl(ctx, protocol.TypeTestEcho, []byte(msg)); err != nil {
			return fmt.Errorf("发送 echo: %w", err)
		}
		select {
		case got := <-echoCh:
			fmt.Printf("发送: %s\n回显: %s\n", msg, got)
		case <-time.After(wait):
			return fmt.Errorf("等待第 %d 条 echo 回显超时（%s）", i, wait)
		case <-sess.Done():
			return fmt.Errorf("会话已关闭: %w", sess.Wait())
		}
	}
	return nil
}

// waitHeartbeat 等待第一次 PONG。心跳未启用时直接返回。
func waitHeartbeat(sess *mux.Session, interval, limit time.Duration) {
	if interval <= 0 {
		return
	}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if sess.PongsReceived() > 0 {
			return
		}
		select {
		case <-sess.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}
