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

// replaceLast 只替换最后一次出现，用于改文件末尾的 server: 段。
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
	if err := cfg.ValidateClient(); err != nil {
		t.Fatalf("common+client 校验失败: %v", err)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("common+server 校验失败: %v", err)
	}

	// common 段
	if cfg.Common.PSK == "" {
		t.Fatal("common.psk 不能为空")
	}
	if got := cfg.Common.KCP.MTU; got != 1350 {
		t.Fatalf("common.kcp.mtu = %d", got)
	}
	if got := cfg.Common.KCP.Interval.D(); got != 10*time.Millisecond {
		t.Fatalf("common.kcp.interval = %s", got)
	}
	if got := cfg.Common.KCP.SndWnd; got != 256 {
		t.Fatalf("common.kcp.sndwnd = %d", got)
	}
	if got := cfg.Common.Stream.IdleTimeout.D(); got != 600*time.Second {
		t.Fatalf("common.stream.idle_timeout = %s", got)
	}
	if cfg.Common.Limits.MaxFrameBody != protocol.MaxFrameBody {
		t.Fatalf("common.limits.max_frame_body = %d", cfg.Common.Limits.MaxFrameBody)
	}
	if cfg.Common.Limits.MaxDataPayload != protocol.MaxDataPayload {
		t.Fatalf("common.limits.max_data_payload = %d", cfg.Common.Limits.MaxDataPayload)
	}
	if cfg.Common.Limits.MaxStreamsPerSession != 256 {
		t.Fatalf("common.limits.max_streams_per_session = %d", cfg.Common.Limits.MaxStreamsPerSession)
	}
	if cfg.Common.CryptName() != "none" {
		t.Fatalf("common.crypt = %q", cfg.Common.CryptName())
	}
	if cfg.Common.AEADName() != protocol.AEADChaCha20Poly1305 {
		t.Fatalf("common.aead = %q", cfg.Common.AEADName())
	}

	// client 段
	if cfg.Client.Listen != "127.0.0.1:2080" {
		t.Fatalf("client.listen = %q", cfg.Client.Listen)
	}
	if cfg.Client.KCP.Server != "127.0.0.1:4000" {
		t.Fatalf("client.kcp.server = %q", cfg.Client.KCP.Server)
	}
	if got := cfg.Client.KCP.HeartbeatInterval.D(); got != 20*time.Second {
		t.Fatalf("client.kcp.heartbeat_interval = %s", got)
	}
	if got := cfg.Client.Pool.StartupJitter.D(); got != 300*time.Millisecond {
		t.Fatalf("client.pool.startup_jitter = %s", got)
	}
	if cfg.Client.Log.LevelOrDefault() != "info" {
		t.Fatalf("client log level = %q", cfg.Client.Log.LevelOrDefault())
	}

	// server 段
	if cfg.Server.Listen != "127.0.0.1:4000" {
		t.Fatalf("server.listen = %q", cfg.Server.Listen)
	}
	if got := cfg.Server.Auth.TimestampWindow.D(); got != time.Minute {
		t.Fatalf("server.auth.timestamp_window = %s", got)
	}
	if cfg.Server.Log.LevelOrDefault() != "info" {
		t.Fatalf("server log level = %q", cfg.Server.Log.LevelOrDefault())
	}

	psk, err := cfg.Common.PSKBytes()
	if err != nil {
		t.Fatalf("common PSK: %v", err)
	}
	if len(psk) != protocol.PSKSize {
		t.Fatalf("PSK 长度 = %d", len(psk))
	}
}

// TestConsistencyFieldsMustBeInCommon 保证「两端必须一致」的字段只存在于 common 段：
// 在 client/server 段里再写一次会因严格模式直接报错，从结构上不可能不一致。
func TestConsistencyFieldsMustBeInCommon(t *testing.T) {
	cases := map[string]string{
		"client.kcp.psk": `client:
  kcp:
    psk: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
`,
		"server.psk": `server:
  psk: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
`,
		"client.kcp.mtu": `client:
  kcp:
    mtu: 1350
`,
		"server.kcp": `server:
  kcp:
    mtu: 1350
`,
		"client.kcp.crypt": `client:
  kcp:
    crypt: "none"
`,
		"server.aead": `server:
  aead: "chacha20-poly1305"
`,
		"client.stream": `client:
  stream:
    idle_timeout: 600s
`,
		"server.limits": `server:
  limits:
    max_frame_body: 65536
`,
		"client.pool.max_streams_per_session": `client:
  pool:
    max_streams_per_session: 256
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadBody(t, body); err == nil {
				t.Fatalf("%s 应属于 common 段，写在别处必须报错", name)
			} else if !strings.Contains(err.Error(), "field") && !strings.Contains(err.Error(), "not found") {
				t.Logf("错误信息: %v", err)
			}
		})
	}
}

func TestLoadStrictUnknownField(t *testing.T) {
	cases := map[string]string{
		"顶层未知字段": `bogus: 1
client:
  listen: "127.0.0.1:2080"
`,
		"common 内未知字段": `common:
  psk: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
  nope: true
`,
		"client 内未知字段": `client:
  listen: "127.0.0.1:2080"
  nope: true
`,
		"common.kcp 内未知字段": `common:
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
	body := strings.Replace(exampleBody(t), "handshake_timeout: 10s", `handshake_timeout: "十秒"`, 1)
	_, err := loadBody(t, body)
	if err == nil {
		t.Fatal("非法 duration 应当报错")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Fatalf("错误信息应提到 duration: %v", err)
	}
}

func TestLoadEmptyFileThenValidateFails(t *testing.T) {
	cfg, err := loadBody(t, "")
	if err != nil {
		t.Fatalf("空文件不应在解析阶段失败: %v", err)
	}
	if err := cfg.ValidateClient(); err == nil {
		t.Fatal("空配置应当校验失败")
	}
	if err := cfg.ValidateServer(); err == nil {
		t.Fatal("空配置应当校验失败")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "not-exist.yaml")); err == nil {
		t.Fatal("文件不存在应当报错")
	}
}

func TestCommonValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{"mtu 越界", func(s string) string { return strings.Replace(s, "mtu: 1350", "mtu: 5000", 1) }, "common.kcp.mtu"},
		{"mtu 过小", func(s string) string { return strings.Replace(s, "mtu: 1350", "mtu: 100", 1) }, "common.kcp.mtu"},
		{"interval 过小", func(s string) string { return strings.Replace(s, "interval: 10ms", "interval: 1ms", 1) }, "common.kcp.interval"},
		{"sndwnd 越界", func(s string) string { return strings.Replace(s, "sndwnd: 256", "sndwnd: 8", 1) }, "common.kcp.sndwnd"},
		{"rcvwnd 越界", func(s string) string { return strings.Replace(s, "rcvwnd: 256", "rcvwnd: 99999", 1) }, "common.kcp.rcvwnd"},
		{"psk 非 base64", func(s string) string {
			return strings.Replace(s, `psk: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="`, `psk: "not-base64!!"`, 1)
		}, "common.psk"},
		{"psk 长度不对", func(s string) string {
			return strings.Replace(s, `psk: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="`, `psk: "c2hvcnQ="`, 1)
		}, "common.psk"},
		{"psk 全 0 占位", func(s string) string {
			return strings.Replace(s, `psk: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="`, `psk: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="`, 1)
		}, "common.psk"},
		{"crypt 未知", func(s string) string { return strings.Replace(s, `crypt: "none"`, `crypt: "rot13"`, 1) }, "common.crypt"},
		{"aead 未知", func(s string) string {
			return strings.Replace(s, `aead: "chacha20-poly1305"`, `aead: "rot13"`, 1)
		}, "common.aead"},
		{"stream.idle_timeout 为 0", func(s string) string {
			return strings.Replace(s, "idle_timeout: 600s", "idle_timeout: 0s", 1)
		}, "common.stream.idle_timeout"},
		{"max_streams_per_session 越界", func(s string) string {
			return strings.Replace(s, "max_streams_per_session: 256", "max_streams_per_session: 999999", 1)
		}, "common.limits.max_streams_per_session"},
		{"max_frame_body 超限", func(s string) string {
			return strings.Replace(s, "max_frame_body: 65536", "max_frame_body: 70000", 1)
		}, "common.limits.max_frame_body"},
		{"max_frame_body 过小", func(s string) string {
			return strings.Replace(s, "max_frame_body: 65536", "max_frame_body: 16", 1)
		}, "common.limits.max_frame_body"},
		{"max_data_payload 过大", func(s string) string {
			return strings.Replace(s, "max_data_payload: 16384", "max_data_payload: 65530", 1)
		}, "common.limits.max_data_payload"},
		{"FEC 只给 data_shards", func(s string) string {
			return strings.Replace(s, "data_shards: 0", "data_shards: 10", 1)
		}, "common.kcp.parity_shards"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadBody(t, tc.mutate(exampleBody(t)))
			if err != nil {
				t.Fatalf("解析阶段不应失败: %v", err)
			}
			err = cfg.Common.Validate()
			if err == nil {
				t.Fatal("应当校验失败")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestClientValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{"listen 缺端口", func(s string) string {
			return strings.Replace(s, `listen: "127.0.0.1:2080"`, `listen: "127.0.0.1"`, 1)
		}, "client.listen"},
		{"server 地址缺主机", func(s string) string {
			return strings.Replace(s, `server: "127.0.0.1:4000"`, `server: ":4000"`, 1)
		}, "client.kcp.server"},
		{"server 端口越界", func(s string) string {
			return strings.Replace(s, `server: "127.0.0.1:4000"`, `server: "127.0.0.1:70000"`, 1)
		}, "client.kcp.server"},
		{"handshake_timeout 为 0", func(s string) string {
			return strings.Replace(s, "handshake_timeout: 10s", "handshake_timeout: 0s", 1)
		}, "client.socks5.handshake_timeout"},
		{"connect_timeout 为 0", func(s string) string {
			return strings.Replace(s, "connect_timeout: 10s", "connect_timeout: 0s", 1)
		}, "client.socks5.connect_timeout"},
		{"pool.size 为 0", func(s string) string { return strings.Replace(s, "size: 2", "size: 0", 1) }, "client.pool.size"},
		{"max_sessions 小于 size", func(s string) string { return strings.Replace(s, "max_sessions: 8", "max_sessions: 1", 1) }, "client.pool.max_sessions"},
		{"pool.idle_timeout 为 0", func(s string) string {
			return strings.Replace(s, "idle_timeout: 300s", "idle_timeout: 0s", 1)
		}, "client.pool.idle_timeout"},
		{"connect_timeout 为 0", func(s string) string {
			return strings.Replace(s, "connect_timeout: 5s", "connect_timeout: 0s", 1)
		}, "client.pool.connect_timeout"},
		{"backoff_max 小于 min", func(s string) string {
			return strings.Replace(s, "backoff_max: 30s", "backoff_max: 100ms", 1)
		}, "client.pool.backoff_max"},
		{"heartbeat_interval 为 0", func(s string) string {
			return strings.Replace(s, "heartbeat_interval: 20s", "heartbeat_interval: 0s", 1)
		}, "client.kcp.heartbeat_interval"},
		{"heartbeat_miss 为 0", func(s string) string {
			return strings.Replace(s, "heartbeat_miss: 3", "heartbeat_miss: 0", 1)
		}, "client.kcp.heartbeat_miss"},
		{"auth_timeout 为 0", func(s string) string {
			return strings.Replace(s, "auth_timeout: 5s", "auth_timeout: 0s", 1)
		}, "client.kcp.auth_timeout"},
		{"auth.mode 未知", func(s string) string { return strings.Replace(s, `mode: "none"`, `mode: "magic"`, 1) }, "client.auth.mode"},
		{"userpass 未实现", func(s string) string { return strings.Replace(s, `mode: "none"`, `mode: "userpass"`, 1) }, "尚未实现"},
		{"log 级别未知", func(s string) string { return strings.Replace(s, `level: "info"`, `level: "trace"`, 1) }, "client.log.level"},
		{"shutdown_grace 为 0", func(s string) string { return strings.Replace(s, "shutdown_grace: 30s", "shutdown_grace: 0s", 1) }, "client.shutdown_grace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadBody(t, tc.mutate(exampleBody(t)))
			if err != nil {
				t.Fatalf("解析阶段不应失败: %v", err)
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
		{"timestamp_window 为 0", func(s string) string {
			return replaceLast(s, "timestamp_window: 60s", "timestamp_window: 0s")
		}, "server.auth.timestamp_window"},
		{"auth_timeout 为 0", func(s string) string {
			return replaceLast(s, "auth_timeout: 5s", "auth_timeout: 0s")
		}, "server.auth.auth_timeout"},
		{"replay_cache_size 为 0", func(s string) string {
			return replaceLast(s, "replay_cache_size: 65536", "replay_cache_size: 0")
		}, "server.auth.replay_cache_size"},
		{"max_pending_sessions 为 0", func(s string) string {
			return replaceLast(s, "max_pending_sessions: 1024", "max_pending_sessions: 0")
		}, "server.auth.max_pending_sessions"},
		{"dial.timeout 为 0", func(s string) string {
			return replaceLast(s, "timeout: 10s", "timeout: 0s")
		}, "server.dial.timeout"},
		{"dial.keepalive 为 0", func(s string) string {
			return replaceLast(s, "keepalive: 30s", "keepalive: 0s")
		}, "server.dial.keepalive"},
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

// TestValidateAggregatesCommonAndOwnSection 验证两段的问题会一次性报出。
func TestValidateAggregatesCommonAndOwnSection(t *testing.T) {
	body := strings.Replace(exampleBody(t), "mtu: 1350", "mtu: 5000", 1)
	body = strings.Replace(body, "size: 2", "size: 0", 1)
	cfg, err := loadBody(t, body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	err = cfg.ValidateClient()
	if err == nil {
		t.Fatal("应当校验失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "common.kcp.mtu") || !strings.Contains(msg, "client.pool.size") {
		t.Fatalf("应同时报出 common 与 client 的问题: %v", err)
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
	var c Common
	if c.CryptName() != "none" {
		t.Fatalf("空 crypt 应规范化为 none，实际 %q", c.CryptName())
	}
	if c.AEADName() != protocol.AEADChaCha20Poly1305 {
		t.Fatalf("空 aead 应规范化，实际 %q", c.AEADName())
	}
	var l Log
	if l.LevelOrDefault() != "info" {
		t.Fatal("空 log level 应默认为 info")
	}
}

func TestLoadKeepsCommonFieldsSingleCopy(t *testing.T) {
	// 同一份 common 段被两个程序共用：解析两次必须得到完全相同的值。
	first, err := Load(examplePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	second, err := Load(examplePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal([]byte(first.Common.PSK), []byte(second.Common.PSK)) {
		t.Fatal("common 段读取结果不一致")
	}
	if first.Common.KCP != second.Common.KCP || first.Common.Limits != second.Common.Limits {
		t.Fatal("common 段读取结果不一致")
	}
}
