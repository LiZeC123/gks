// Package log 提供 gks 统一的结构化日志构造。
//
// 日志只写文件（或丢弃），不再写控制台：控制台留给周期刷新的统计表格。
package log

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// 结构化字段名（dev.md §11.9）。
const (
	SessionID = "session_id"
	StreamID  = "stream_id"
	Remote    = "remote"
	Target    = "target"
	Event     = "event"
)

// ParseLevel 把配置里的级别字符串转成 slog 级别。空值视为 info。
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("log: 未知日志级别 %q", s)
	}
}

// New 构造写往 w 的文本日志器。
func New(w io.Writer, level string) (*slog.Logger, error) {
	lv, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv})), nil
}

// NewFromConfig 按配置构造日志器：
//
//   - file 为空 → 丢弃全部日志（不写任何文件，也不写控制台）；
//   - file 非空 → 以追加方式写入该文件（父目录不存在时自动创建）。
//
// 返回的 Closer 用于退出时关闭文件；不写文件时是一个空操作。
func NewFromConfig(level, file string) (*slog.Logger, io.Closer, error) {
	if strings.TrimSpace(file) == "" {
		l, err := New(io.Discard, level)
		if err != nil {
			return nil, nil, err
		}
		return l, nopCloser{}, nil
	}
	f, err := openLogFile(file)
	if err != nil {
		return nil, nil, err
	}
	lv, err := ParseLevel(level)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: lv})), f, nil
}

// openLogFile 打开日志文件：追加写，父目录不存在时自动创建。
func openLogFile(path string) (*os.File, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建日志目录 %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件 %s: %w", path, err)
	}
	return f, nil
}

// nopCloser 是「不写文件」时的空操作 Closer，避免调用方到处判空。
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// WithSession 返回带 session_id 与 remote 字段的 logger。
func WithSession(l *slog.Logger, conv uint32, remote string) *slog.Logger {
	if l == nil {
		l = slog.Default()
	}
	return l.With(SessionID, conv, Remote, remote)
}
