// Package config 负责加载与校验 gks 的配置文件（dev.md §7）。
//
// 形态：单个 YAML 文件，顶层分为 client: 与 server: 两段，
// 两个程序各自 -c 同一文件，各取所需段落。解析使用严格模式（未知字段即报错）。
package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/LiZeC123/gks/internal/protocol"
)

// Duration 是支持 YAML 中 Go duration 字符串（"10s"、"300ms"）的时长类型。
type Duration time.Duration

// UnmarshalYAML 解析 duration 字符串。
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("需要 duration 字符串（如 10s）: %w", err)
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("非法 duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// D 返回标准库时长。
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config 是整个配置文件。
type Config struct {
	Client Client `yaml:"client"`
	Server Server `yaml:"server"`
}

// Client 是 client: 段。
type Client struct {
	Listen        string     `yaml:"listen"`
	Auth          ClientAuth `yaml:"auth"`
	Socks5        Socks5     `yaml:"socks5"`
	Pool          Pool       `yaml:"pool"`
	KCP           ClientKCP  `yaml:"kcp"`
	Stream        Stream     `yaml:"stream"`
	ShutdownGrace Duration   `yaml:"shutdown_grace"`
	Log           Log        `yaml:"log"`
}

// ClientAuth 是本地 SOCKS5 的认证设置。本版仅支持 none。
type ClientAuth struct {
	Mode  string     `yaml:"mode"`
	Users []UserPass `yaml:"users"`
}

// UserPass 是 userpass 认证的用户条目（本版尚未实现）。
type UserPass struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Socks5 是本地 SOCKS5 服务设置。
type Socks5 struct {
	HandshakeTimeout Duration `yaml:"handshake_timeout"`
}

// Pool 是 KCP Session 池设置。
type Pool struct {
	Size                 int      `yaml:"size"`
	MaxSessions          int      `yaml:"max_sessions"`
	MaxStreamsPerSession int      `yaml:"max_streams_per_session"`
	IdleTimeout          Duration `yaml:"idle_timeout"`
	StartupJitter        Duration `yaml:"startup_jitter"`
	ConnectTimeout       Duration `yaml:"connect_timeout"`
	BackoffMin           Duration `yaml:"backoff_min"`
	BackoffMax           Duration `yaml:"backoff_max"`
}

// KCPTuning 是两端共用的 KCP 调参。
type KCPTuning struct {
	Interval     Duration `yaml:"interval"`
	MTU          int      `yaml:"mtu"`
	SndWnd       int      `yaml:"sndwnd"`
	RcvWnd       int      `yaml:"rcvwnd"`
	DataShards   int      `yaml:"data_shards"`
	ParityShards int      `yaml:"parity_shards"`
}

// ClientKCP 是 client 侧的 KCP 与认证设置。
type ClientKCP struct {
	KCPTuning         `yaml:",inline"`
	Server            string   `yaml:"server"`
	PSK               string   `yaml:"psk"`
	Crypt             string   `yaml:"crypt"`
	AEAD              string   `yaml:"aead"`
	HeartbeatInterval Duration `yaml:"heartbeat_interval"`
	HeartbeatMiss     int      `yaml:"heartbeat_miss"`
	AuthTimeout       Duration `yaml:"auth_timeout"`
}

// Stream 是流级设置。
type Stream struct {
	IdleTimeout Duration `yaml:"idle_timeout"`
}

// Log 是日志设置。
type Log struct {
	Level string `yaml:"level"`
}

// Server 是 server: 段。
type Server struct {
	Listen        string     `yaml:"listen"`
	PSK           string     `yaml:"psk"`
	Crypt         string     `yaml:"crypt"`
	AEAD          string     `yaml:"aead"`
	Auth          ServerAuth `yaml:"auth"`
	KCP           KCPTuning  `yaml:"kcp"`
	Dial          Dial       `yaml:"dial"`
	Stream        Stream     `yaml:"stream"`
	Limits        Limits     `yaml:"limits"`
	ShutdownGrace Duration   `yaml:"shutdown_grace"`
	Log           Log        `yaml:"log"`
}

// ServerAuth 是服务端认证设置。
type ServerAuth struct {
	TimestampWindow    Duration `yaml:"timestamp_window"`
	AuthTimeout        Duration `yaml:"auth_timeout"`
	ReplayCacheSize    int      `yaml:"replay_cache_size"`
	MaxPendingSessions int      `yaml:"max_pending_sessions"`
}

// Dial 是服务端拨号设置。
type Dial struct {
	Timeout   Duration `yaml:"timeout"`
	Keepalive Duration `yaml:"keepalive"`
}

// Limits 是服务端限额。
type Limits struct {
	MaxStreamsPerSession int `yaml:"max_streams_per_session"`
	MaxFrameBody         int `yaml:"max_frame_body"`
	MaxDataPayload       int `yaml:"max_data_payload"`
}

// 取值范围常量（dev.md §7.3）。
const (
	MinMTU            = 576
	MaxMTU            = 1400
	MinInterval       = 5 * time.Millisecond
	MaxInterval       = 100 * time.Millisecond
	MinWindow         = 32
	MaxWindow         = 4096
	MinPoolSize       = 1
	MaxPoolSize       = 16
	MaxStreamsPerSess = 32768
	MaxPoolSessions   = 16
	AllowedCryptNames = "none|aes-128-gcm|aes-256-gcm|aes-128|aes-256|salsa20"
	LocalAuthNone     = "none"
	LocalAuthUserPass = "userpass"
)

// Load 读取并严格解析配置文件。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			// 空文件：交给 Validate 报出可读的缺项错误。
			return &Config{}, nil
		}
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	return &cfg, nil
}

// problems 收集校验问题，最后用 errors.Join 一次性返回，便于一次改完所有错。
type problems struct {
	errs []error
}

func (p *problems) add(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

func (p *problems) require(cond bool, format string, args ...any) {
	if !cond {
		p.add(format, args...)
	}
}

func (p *problems) err() error { return errors.Join(p.errs...) }

// Validate 校验 client: 段。
func (c *Client) Validate() error {
	p := &problems{}
	validateAddr(p, "client.listen", c.Listen, false)
	validateAddr(p, "client.kcp.server", c.KCP.Server, true)
	validateKCPTuning(p, "client.kcp", &c.KCP.KCPTuning)
	validateCrypt(p, "client.kcp.crypt", c.KCP.Crypt)
	validateAEAD(p, "client.kcp.aead", c.KCP.AEAD)
	decodePSK(p, "client.kcp.psk", c.KCP.PSK)

	p.require(c.Auth.Mode == LocalAuthNone || c.Auth.Mode == LocalAuthUserPass,
		"client.auth.mode: 只能是 %q 或 %q，实际 %q", LocalAuthNone, LocalAuthUserPass, c.Auth.Mode)
	if c.Auth.Mode == LocalAuthUserPass {
		p.add("client.auth.mode: userpass 认证尚未实现（见 dev.md §16.1）")
	}
	p.require(len(c.Auth.Users) == 0, "client.auth.users: userpass 尚未实现，必须为空")
	p.require(c.Auth.Mode != "" || len(c.Auth.Users) == 0, "client.auth.users: userpass 尚未实现，必须为空")

	p.require(c.Socks5.HandshakeTimeout.D() > 0, "client.socks5.handshake_timeout: 必须大于 0")
	p.require(c.Pool.Size >= MinPoolSize && c.Pool.Size <= MaxPoolSize,
		"client.pool.size: 需在 [%d,%d]，实际 %d", MinPoolSize, MaxPoolSize, c.Pool.Size)
	p.require(c.Pool.MaxSessions >= MinPoolSize && c.Pool.MaxSessions <= MaxPoolSessions,
		"client.pool.max_sessions: 需在 [%d,%d]，实际 %d", MinPoolSize, MaxPoolSessions, c.Pool.MaxSessions)
	p.require(c.Pool.MaxSessions >= c.Pool.Size,
		"client.pool.max_sessions: 必须不小于 pool.size（%d < %d）", c.Pool.MaxSessions, c.Pool.Size)
	p.require(c.Pool.MaxStreamsPerSession >= 1 && c.Pool.MaxStreamsPerSession <= MaxStreamsPerSess,
		"client.pool.max_streams_per_session: 需在 [1,%d]，实际 %d", MaxStreamsPerSess, c.Pool.MaxStreamsPerSession)
	p.require(c.Pool.IdleTimeout.D() > 0, "client.pool.idle_timeout: 必须大于 0")
	p.require(c.Pool.StartupJitter.D() >= 0, "client.pool.startup_jitter: 不能为负")
	p.require(c.Pool.ConnectTimeout.D() > 0, "client.pool.connect_timeout: 必须大于 0")
	p.require(c.Pool.BackoffMin.D() > 0, "client.pool.backoff_min: 必须大于 0")
	p.require(c.Pool.BackoffMax.D() >= c.Pool.BackoffMin.D(),
		"client.pool.backoff_max: 不能小于 backoff_min")

	p.require(c.KCP.HeartbeatInterval.D() > 0, "client.kcp.heartbeat_interval: 必须大于 0")
	p.require(c.KCP.HeartbeatMiss >= 1, "client.kcp.heartbeat_miss: 必须不小于 1")
	p.require(c.KCP.AuthTimeout.D() > 0, "client.kcp.auth_timeout: 必须大于 0")

	p.require(c.Stream.IdleTimeout.D() > 0, "client.stream.idle_timeout: 必须大于 0")
	p.require(c.ShutdownGrace.D() > 0, "client.shutdown_grace: 必须大于 0")
	validateLevel(p, "client.log.level", c.Log.Level)
	return p.err()
}

// Validate 校验 server: 段。
func (s *Server) Validate() error {
	p := &problems{}
	validateAddr(p, "server.listen", s.Listen, false)
	validateKCPTuning(p, "server.kcp", &s.KCP)
	validateCrypt(p, "server.crypt", s.Crypt)
	validateAEAD(p, "server.aead", s.AEAD)
	decodePSK(p, "server.psk", s.PSK)

	p.require(s.Auth.TimestampWindow.D() > 0, "server.auth.timestamp_window: 必须大于 0")
	p.require(s.Auth.AuthTimeout.D() > 0, "server.auth.auth_timeout: 必须大于 0")
	p.require(s.Auth.ReplayCacheSize > 0, "server.auth.replay_cache_size: 必须大于 0")
	p.require(s.Auth.MaxPendingSessions > 0, "server.auth.max_pending_sessions: 必须大于 0")

	p.require(s.Dial.Timeout.D() > 0, "server.dial.timeout: 必须大于 0")
	p.require(s.Dial.Keepalive.D() > 0, "server.dial.keepalive: 必须大于 0")
	p.require(s.Stream.IdleTimeout.D() > 0, "server.stream.idle_timeout: 必须大于 0")

	p.require(s.Limits.MaxStreamsPerSession >= 1 && s.Limits.MaxStreamsPerSession <= MaxStreamsPerSess,
		"server.limits.max_streams_per_session: 需在 [1,%d]，实际 %d", MaxStreamsPerSess, s.Limits.MaxStreamsPerSession)
	p.require(s.Limits.MaxFrameBody > 0 && s.Limits.MaxFrameBody <= protocol.MaxFrameBody,
		"server.limits.max_frame_body: 需在 (0,%d]，实际 %d", protocol.MaxFrameBody, s.Limits.MaxFrameBody)
	minBody := protocol.BodyPrefixSize + protocol.TagSize
	p.require(s.Limits.MaxFrameBody > minBody,
		"server.limits.max_frame_body: 必须大于 %d，实际 %d", minBody, s.Limits.MaxFrameBody)
	p.require(s.Limits.MaxDataPayload >= 1, "server.limits.max_data_payload: 必须不小于 1")
	p.require(s.Limits.MaxDataPayload <= s.Limits.MaxFrameBody-minBody,
		"server.limits.max_data_payload: 不能超过 max_frame_body-%d（%d > %d）",
		minBody, s.Limits.MaxDataPayload, s.Limits.MaxFrameBody-minBody)

	p.require(s.ShutdownGrace.D() > 0, "server.shutdown_grace: 必须大于 0")
	validateLevel(p, "server.log.level", s.Log.Level)
	return p.err()
}

// PSKBytes 返回 client 侧解码后的 PSK。
func (c *Client) PSKBytes() ([]byte, error) {
	psk, err := decodeBase64PSK(c.KCP.PSK)
	if err != nil {
		return nil, fmt.Errorf("client.kcp.psk: %w", err)
	}
	return psk, nil
}

// PSKBytes 返回 server 侧解码后的 PSK。
func (s *Server) PSKBytes() ([]byte, error) {
	psk, err := decodeBase64PSK(s.PSK)
	if err != nil {
		return nil, fmt.Errorf("server.psk: %w", err)
	}
	return psk, nil
}

// CryptName 返回规范化后的传输层加密算法名（空值即 none）。
func (c *Client) CryptName() string { return normalizeCrypt(c.KCP.Crypt) }

// CryptName 返回规范化后的传输层加密算法名（空值即 none）。
func (s *Server) CryptName() string { return normalizeCrypt(s.Crypt) }

// AEADName 返回规范化后的应用层 AEAD 算法名。
func (c *Client) AEADName() string { return normalizeAEAD(c.KCP.AEAD) }

// AEADName 返回规范化后的应用层 AEAD 算法名。
func (s *Server) AEADName() string { return normalizeAEAD(s.AEAD) }

// Level 返回规范化后的日志级别（空值即 info）。
func (l Log) LevelOrDefault() string {
	if l.Level == "" {
		return "info"
	}
	return l.Level
}

func normalizeCrypt(name string) string {
	if name == "" {
		return "none"
	}
	return name
}

func normalizeAEAD(name string) string {
	if name == "" {
		return protocol.AEADChaCha20Poly1305
	}
	return name
}

// ValidCrypt 报告传输层加密算法名是否受支持。
func ValidCrypt(name string) bool {
	switch normalizeCrypt(name) {
	case "none", "aes-128", "aes-256", "salsa20", "aes-128-gcm", "aes-256-gcm":
		return true
	default:
		return false
	}
}

func validateCrypt(p *problems, prefix, name string) {
	p.require(ValidCrypt(name), "%s: 只能是 %s，实际 %q", prefix, AllowedCryptNames, name)
}

func validateAEAD(p *problems, prefix, name string) {
	p.require(name == "" || protocol.ValidAEAD(name), "%s: 只能是 %q 或 %q，实际 %q",
		prefix, protocol.AEADChaCha20Poly1305, protocol.AEADAES256GCM, name)
}

func validateKCPTuning(p *problems, prefix string, k *KCPTuning) {
	p.require(k.MTU >= MinMTU && k.MTU <= MaxMTU,
		"%s.mtu: 需在 [%d,%d]，实际 %d", prefix, MinMTU, MaxMTU, k.MTU)
	p.require(k.Interval.D() >= MinInterval && k.Interval.D() <= MaxInterval,
		"%s.interval: 需在 [%s,%s]，实际 %s", prefix, MinInterval, MaxInterval, k.Interval.D())
	p.require(k.SndWnd >= MinWindow && k.SndWnd <= MaxWindow,
		"%s.sndwnd: 需在 [%d,%d]，实际 %d", prefix, MinWindow, MaxWindow, k.SndWnd)
	p.require(k.RcvWnd >= MinWindow && k.RcvWnd <= MaxWindow,
		"%s.rcvwnd: 需在 [%d,%d]，实际 %d", prefix, MinWindow, MaxWindow, k.RcvWnd)
	p.require(k.DataShards >= 0, "%s.data_shards: 不能为负", prefix)
	p.require(k.ParityShards >= 0, "%s.parity_shards: 不能为负", prefix)
	p.require(k.DataShards == 0 || k.ParityShards > 0,
		"%s.parity_shards: 启用 FEC 时 parity_shards 必须大于 0", prefix)
}

func validateLevel(p *problems, prefix, level string) {
	switch level {
	case "", "debug", "info", "warn", "error":
	default:
		p.add("%s: 只能是 debug|info|warn|error，实际 %q", prefix, level)
	}
}

func validateAddr(p *problems, prefix, addr string, requireHost bool) {
	if addr == "" {
		p.add("%s: 不能为空", prefix)
		return
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		p.add("%s: 需要 host:port 形式（%q）: %v", prefix, addr, err)
		return
	}
	if requireHost && host == "" {
		p.add("%s: 必须指定主机（%q）", prefix, addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		p.add("%s: 端口必须在 1..65535，实际 %q", prefix, port)
	}
}

func decodePSK(p *problems, prefix, s string) []byte {
	psk, err := decodeBase64PSK(s)
	if err != nil {
		p.add("%s: %v", prefix, err)
		return nil
	}
	return psk
}

func decodeBase64PSK(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("不能为空（需要 base64 编码的 32 字节密钥）")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("不是合法的 base64: %w", err)
	}
	if len(raw) != protocol.PSKSize {
		return nil, fmt.Errorf("解码后必须是 %d 字节，实际 %d", protocol.PSKSize, len(raw))
	}
	allZero := true
	for _, b := range raw {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return nil, errors.New("不能是全 0 的占位值，请生成真实密钥")
	}
	return raw, nil
}
