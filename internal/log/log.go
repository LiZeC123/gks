// Package log 提供 gks 统一的结构化日志构造。
package log

import (
	"fmt"
	"io"
	"log/slog"
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

// WithSession 返回带 session_id 与 remote 字段的 logger。
func WithSession(l *slog.Logger, conv uint32, remote string) *slog.Logger {
	if l == nil {
		l = slog.Default()
	}
	return l.With(SessionID, conv, Remote, remote)
}
