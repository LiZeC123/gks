package log

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]bool{
		"":      true,
		"INFO":  true,
		"debug": true,
		"warn":  true,
		"error": true,
		"trace": false,
		"xx":    false,
	}
	for in, ok := range cases {
		if _, err := ParseLevel(in); (err == nil) != ok {
			t.Fatalf("ParseLevel(%q) 的成败不符合预期（err=%v）", in, err)
		}
	}
}

// TestNewFromConfigWithoutFileDiscards 验证「不写日志文件」时日志被完全丢弃，
// 且 Closer 可以安全关闭（调用方无需判空）。
func TestNewFromConfigWithoutFileDiscards(t *testing.T) {
	for _, file := range []string{"", "   "} {
		logger, closer, err := NewFromConfig("info", file)
		if err != nil {
			t.Fatalf("NewFromConfig(%q): %v", file, err)
		}
		logger.Info("不该出现在任何地方", Event, "should_not_appear")
		if err := closer.Close(); err != nil {
			t.Fatalf("关闭: %v", err)
		}
	}
}

// TestNewFromConfigCreatesParentDirsAndAppends 验证父目录自动创建与追加写。
func TestNewFromConfigCreatesParentDirsAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "logs", "gks.log")

	logger, closer, err := NewFromConfig("info", path)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	logger.Info("第一次", Event, "first_run")
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	logger2, closer2, err := NewFromConfig("info", path)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	logger2.Info("第二次", Event, "second_run")
	if err := closer2.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志: %v", err)
	}
	out := string(raw)
	if !strings.Contains(out, "event=first_run") || !strings.Contains(out, "event=second_run") {
		t.Fatalf("日志应当是追加写:\n%s", out)
	}
}

func TestNewFromConfigRespectsLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gks.log")
	logger, closer, err := NewFromConfig("error", path)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	logger.Info("info 不该写入", Event, "info_line")
	logger.Error("error 应写入", Event, "error_line")
	_ = closer.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志: %v", err)
	}
	out := string(raw)
	if strings.Contains(out, "info_line") || !strings.Contains(out, "error_line") {
		t.Fatalf("级别过滤不对:\n%s", out)
	}
}

func TestNewFromConfigBadPath(t *testing.T) {
	// 路径本身是目录：打开失败应当直接报错，而不是静默丢弃日志。
	if _, _, err := NewFromConfig("info", t.TempDir()); err == nil {
		t.Fatal("目录作为日志文件应当报错")
	}
	if _, _, err := NewFromConfig("bogus", filepath.Join(t.TempDir(), "gks.log")); err == nil {
		t.Fatal("非法级别应当报错")
	}
}
