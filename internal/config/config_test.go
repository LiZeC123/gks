package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LiZeC123/gks/internal/protocol"
)

const examplePath = "../../test/gks.yaml.example"

func exampleBody(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("读取示例配置: %v", err)
	}
	return string(b)
}

// replaceLast 只替换最后一次出现，用于只改 server: 段（它在文件末尾）。
func replaceLast(s, old, new string) string {
	i := strings.LastIndex(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gks.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("写入临时配置: %v", err)
	}
	return p
}

func loadBody(t *testing.T, body string) (*Config, error) {
	t.Helper()
	return Load(writeConfig(t, body))
}

// TestExampleConfigIsValid 保证随仓库分发的示例配置始终能通过校验。
func TestExampleConfigIsValid(t *testing.T) {
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Client.Validate(); err != nil {
		t.Fatalf("client 段校验失败: %v", err)
	}
	if err := cfg.Server.Validate(); err != nil {
		t.Fatalf("server 段校验失败: %v", err)
	}

	if cfg.Client.Listen != "127.0.0.1:1080" {
		t.Fatalf("client.listen = %q", cfg.Client.Listen)
	}
	if cfg.Server.Listen != "127.0.0.1:4000" {
		t.Fatalf("server.listen = %q", cfg.Server.Listen)
	}
	if got := cfg.Client.KCP.HeartbeatInterval.D(); got != 20*time.Second {
		t.Fatalf("heartbeat_interval = %s", got)
	}
	if got := cfg.Client.Pool.StartupJitter.D(); got != 300*time.Millisecond {
		t.Fatalf("startup_jitter = %s", got)
	}
	if got := cfg.Server.Auth.TimestampWindow.D(); got != time.Minute {
		t.Fatalf("timestamp_window = %s", got)
	}
	if cfg.Client.CryptName() != "none" || cfg.Server.CryptName() != "none" {
		t.Fatal("示例配置的 crypt 应为 none（阶段 B）")
	}
	if cfg.Client.AEADName() != protocol.AEADChaCha20Poly1305 {
		t.Fatalf("默认 aead = %q", cfg.Client.AEADName())
	}
	if cfg.Server.Limits.MaxFrameBody != protocol.MaxFrameBody {
		t.Fatalf("max_frame_body = %d", cfg.Server.Limits.MaxFrameBody)
	}
	if cfg.Server.Limits.MaxDataPayload != protocol.MaxDataPayload {
		t.Fatalf("max_data_payload = %d", cfg.Server.Limits.MaxDataPayload)
	}
	if cfg.Client.Log.LevelOrDefault() != "info" {
		t.Fatalf("client log level = %q", cfg.Client.Log.LevelOrDefault())
	}

	cpsk, err := cfg.Client.PSKBytes()
	if err != nil {
		t.Fatalf("client PSK: %v", err)
	}
	spsk, err := cfg.Server.PSKBytes()
	if err != nil {
		t.Fatalf("server PSK: %v", err)
	}
	if len(cpsk) != protocol.PSKSize || !bytes.Equal(cpsk, spsk) {
		t.Fatal("两端 PSK 应一致且为 32 字节")
	}
}

func TestLoadStrictUnknownField(t *testing.T) {
	cases := map[string]string{
		"顶层未知字段": `bogus: 1
client:
  listen: "127.0.0.1:1080"
`,
		"嵌套未知字段": `client:
  listen: "127.0.0.1:1080"
  nope: true
`,
		"kcp 内未知字段": `client:
  kcp:
    window: 10
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadBody(t, body); err == nil {
				t.Fatal("未知字段应当报错")
			}
		})
	}
}

func TestLoadBadDuration(t *testing.T) {
	_, err := loadBody(t, exampleBody(t)+"\n")
	// 在示例基础上制造一个非法 duration。
	body := strings.Replace(exampleBody(t), "handshake_timeout: 10s", `handshake_timeout: "十秒"`, 1)
	if _, err = loadBody(t, body); err == nil {
		t.Fatal("非法 duration 应当报错")
	} else if !strings.Contains(err.Error(), "duration") {
		t.Fatalf("错误信息应提到 duration: %v", err)
	}
}

func TestLoadEmptyFileThenValidateFails(t *testing.T) {
	cfg, err := loadBody(t, "")
	if err != nil {
		t.Fatalf("空文件不应在解析阶段失败: %v", err)
	}
	if err := cfg.Client.Validate(); err == nil {
		t.Fatal("空配置应当校验失败")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "not-exist.yaml")); err == nil {
		t.Fatal("文件不存在应当报错")
	}
}

func TestClientValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{"mtu 越界", func(s string) string { return strings.Replace(s, "mtu: 1350", "mtu: 5000", 1) }, "client.kcp.mtu"},
		{"mtu 过小", func(s string) string { return strings.Replace(s, "mtu: 1350", "mtu: 100", 1) }, "client.kcp.mtu"},
		{"interval 过小", func(s string) string { return strings.Replace(s, "interval: 10ms", "interval: 1ms", 1) }, "client.kcp.interval"},
		{"sndwnd 越界", func(s string) string { return strings.Replace(s, "sndwnd: 256", "sndwnd: 8", 1) }, "client.kcp.sndwnd"},
		{"rcvwnd 越界", func(s string) string { return strings.Replace(s, "rcvwnd: 256", "rcvwnd: 99999", 1) }, "client.kcp.rcvwnd"},
		{"pool.size 为 0", func(s string) string { return strings.Replace(s, "size: 2", "size: 0", 1) }, "client.pool.size"},
		{"max_sessions 小于 size", func(s string) string { return strings.Replace(s, "max_sessions: 8", "max_sessions: 1", 1) }, "client.pool.max_sessions"},
		{"connnect_timeout 为 0", func(s string) string { return strings.Replace(s, "connect_timeout: 5s", "connect_timeout: 0s", 1) }, "client.pool.connect_timeout"},
		{"backoff_max 小于 min", func(s string) string { return strings.Replace(s, "backoff_max: 30s", "backoff_max: 100ms", 1) }, "client.pool.backoff_max"},
		{"heartbeat_miss 为 0", func(s string) string { return strings.Replace(s, "heartbeat_miss: 3", "heartbeat_miss: 0", 1) }, "client.kcp.heartbeat_miss"},
		{"psk 非 base64", func(s string) string {
			return strings.Replace(s, `psk: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="`, `psk: "not-base64!!"`, 1)
		}, "client.kcp.psk"},
		{"psk 长度不对", func(s string) string {
			return strings.Replace(s, `psk: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="`, `psk: "c2hvcnQ="`, 1)
		}, "client.kcp.psk"},
		{"psk 全 0 占位", func(s string) string {
			return strings.Replace(s, `psk: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="`, `psk: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="`, 1)
		}, "client.kcp.psk"},
		{"crypt 未知", func(s string) string { return strings.Replace(s, `crypt: "none"`, `crypt: "rot13"`, 1) }, "client.kcp.crypt"},
		{"aead 未知", func(s string) string { return strings.Replace(s, `aead: "chacha20-poly1305"`, `aead: "rot13"`, 1) }, "client.kcp.aead"},
		{"listen 缺端口", func(s string) string {
			return strings.Replace(s, `listen: "127.0.0.1:1080"`, `listen: "127.0.0.1"`, 1)
		}, "client.listen"},
		{"server 地址缺主机", func(s string) string {
			return strings.Replace(s, `server: "127.0.0.1:4000"`, `server: ":4000"`, 1)
		}, "client.kcp.server"},
		{"端口越界", func(s string) string {
			return strings.Replace(s, `server: "127.0.0.1:4000"`, `server: "127.0.0.1:70000"`, 1)
		}, "client.kcp.server"},
		{"auth.mode 未知", func(s string) string { return strings.Replace(s, `mode: "none"`, `mode: "magic"`, 1) }, "client.auth.mode"},
		{"userpass 未实现", func(s string) string { return strings.Replace(s, `mode: "none"`, `mode: "userpass"`, 1) }, "尚未实现"},
		{"log 级别未知", func(s string) string { return strings.Replace(s, `level: "info"`, `level: "trace"`, 1) }, "log.level"},
		{"shutdown_grace 为 0", func(s string) string { return strings.Replace(s, "shutdown_grace: 30s", "shutdown_grace: 0s", 1) }, "client.shutdown_grace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadBody(t, tc.mutate(exampleBody(t)))
			if err != nil {
				t.Fatalf("解析阶段就不应失败: %v", err)
			}
			err = cfg.Client.Validate()
			if err == nil {
				t.Fatal("应当校验失败")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestClientUserPassUsersMustBeEmpty(t *testing.T) {
	body := strings.Replace(exampleBody(t), "users: []", `users:
      - username: u
        password: p`, 1)
	cfg, err := loadBody(t, body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := cfg.Client.Validate(); err == nil {
		t.Fatal("userpass 未实现时 users 非空应当报错")
	}
}

func TestServerValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{"listen 缺端口", func(s string) string {
			return replaceLast(s, `listen: "127.0.0.1:4000"`, `listen: "127.0.0.1"`)
		}, "server.listen"},
		{"psk 缺失", func(s string) string {
			return replaceLast(s, `psk: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="`, `psk: ""`)
		}, "server.psk"},
		{"crypt 未知", func(s string) string { return replaceLast(s, `crypt: "none"`, `crypt: "rc4"`) }, "server.crypt"},
		{"aead 未知", func(s string) string {
			return replaceLast(s, `aead: "chacha20-poly1305"`, `aead: "rc4"`)
		}, "server.aead"},
		{"timestamp_window 为 0", func(s string) string {
			return replaceLast(s, "timestamp_window: 60s", "timestamp_window: 0s")
		}, "server.auth.timestamp_window"},
		{"replay_cache_size 为 0", func(s string) string {
			return replaceLast(s, "replay_cache_size: 65536", "replay_cache_size: 0")
		}, "server.auth.replay_cache_size"},
		{"max_pending_sessions 为 0", func(s string) string {
			return replaceLast(s, "max_pending_sessions: 1024", "max_pending_sessions: 0")
		}, "server.auth.max_pending_sessions"},
		{"dial.timeout 为 0", func(s string) string {
			return replaceLast(s, "timeout: 10s", "timeout: 0s")
		}, "server.dial.timeout"},
		{"max_frame_body 超限", func(s string) string {
			return replaceLast(s, "max_frame_body: 65536", "max_frame_body: 70000")
		}, "server.limits.max_frame_body"},
		{"max_data_payload 过大", func(s string) string {
			return replaceLast(s, "max_data_payload: 16384", "max_data_payload: 65530")
		}, "server.limits.max_data_payload"},
		{"max_streams_per_session 越界", func(s string) string {
			return replaceLast(s, "max_streams_per_session: 256", "max_streams_per_session: 999999")
		}, "server.limits.max_streams_per_session"},
		{"FEC 只给 data_shards", func(s string) string {
			return replaceLast(s, "data_shards: 0", "data_shards: 10")
		}, "server.kcp.parity_shards"},
		{"log 级别未知", func(s string) string {
			return replaceLast(s, `level: "info"`, `level: "verbose"`)
		}, "server.log.level"},
		{"shutdown_grace 为 0", func(s string) string {
			return replaceLast(s, "shutdown_grace: 30s", "shutdown_grace: 0s")
		}, "server.shutdown_grace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadBody(t, tc.mutate(exampleBody(t)))
			if err != nil {
				t.Fatalf("解析阶段不应失败: %v", err)
			}
			err = cfg.Server.Validate()
			if err == nil {
				t.Fatal("应当校验失败")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidCrypt(t *testing.T) {
	for _, name := range []string{"", "none", "aes-128-gcm", "aes-256-gcm", "aes-128", "aes-256", "salsa20"} {
		if !ValidCrypt(name) {
			t.Fatalf("%q 应受支持", name)
		}
	}
	for _, name := range []string{"rc4", "chacha20", "aes"} {
		if ValidCrypt(name) {
			t.Fatalf("%q 不应受支持", name)
		}
	}
}

func TestNormalizeDefaults(t *testing.T) {
	var c Client
	if c.CryptName() != "none" {
		t.Fatalf("空 crypt 应规范化为 none，实际 %q", c.CryptName())
	}
	if c.AEADName() != protocol.AEADChaCha20Poly1305 {
		t.Fatalf("空 aead 应规范化，实际 %q", c.AEADName())
	}
	var s Server
	if s.CryptName() != "none" || s.AEADName() != protocol.AEADChaCha20Poly1305 {
		t.Fatal("server 侧默认值不正确")
	}
	var l Log
	if l.LevelOrDefault() != "info" {
		t.Fatal("空 log level 应默认为 info")
	}
}
