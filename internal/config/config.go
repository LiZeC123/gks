// Package config 负责加载与校验 gks 的配置文件（dev.md §7）。
//
// 形态：单个 YAML 文件，顶层分为三段：
//
//	common: 两端必须一致的参数（PSK、加密算法、KCP 调参、流与帧上限）——只写一次
//	client: 仅客户端使用的参数
//	server: 仅服务端使用的参数
//
// 两个程序各自 -c 同一文件，各取所需段落。把「必须一致」的字段集中在 common，
// 是为了从结构上消除两端各写一份导致的不一致（psk/crypt/aead/窗口/帧上限等）。
// 解析使用严格模式：出现未知字段即报错。
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
	Common Common `yaml:"common"`
	Client Client `yaml:"client"`
	Server Server `yaml:"server"`
}

// Common 是两端必须保持一致的参数。
//
// 这些字段一旦两端不一致，轻则行为诡异（窗口/MTU 不匹配），重则直接不可用
// （PSK/crypt/aead 不一致会导致握手或解密失败），因此只在 common 段写一次。
type Common struct {
	// PSK 是 base64 编码的 32 字节预共享密钥。
	PSK   string `yaml:"psk"`
	Crypt string `yaml:"crypt"`
	AEAD  string `yaml:"aead"`
	// KCP 是两端的 KCP 调参。
	KCP KCPTuning `yaml:"kcp"`
	// Stream 是流级策略（两端对「流空闲多久算死」的口径一致）。
	Stream Stream `yaml:"stream"`
	// Limits 是帧与流的上限：发送方必须遵守，接收方据此校验。
	Limits Limits `yaml:"limits"`
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

// Stream 是流级策略。
type Stream struct {
	IdleTimeout Duration `yaml:"idle_timeout"`
}

// Limits 是单会话的上限。
type Limits struct {
	// MaxStreamsPerSession 既是客户端单会话的开流上限，也是服务端的接受上限。
	MaxStreamsPerSession int `yaml:"max_streams_per_session"`
	// MaxFrameBody 是单帧帧体上限，超过即断开（防伪造长度挂起）。
	MaxFrameBody int `yaml:"max_frame_body"`
	// MaxDataPayload 是单个 DATA 帧的 Payload 上限，发送方据此拆帧。
	MaxDataPayload int `yaml:"max_data_payload"`
}

// Client 是 client: 段（只放客户端独有的参数）。
type Client struct {
	Listen        string     `yaml:"listen"`
	Auth          ClientAuth `yaml:"auth"`
	Socks5        Socks5     `yaml:"socks5"`
	Pool          Pool       `yaml:"pool"`
	KCP           ClientKCP  `yaml:"kcp"`
	ShutdownGrace Duration   `yaml:"shutdown_grace"`
	Log           Log        `yaml:"log"`
}

// ClientKCP 是客户端侧的连接与保活参数（KCP 调参在 common.kcp）。
type ClientKCP struct {
	// Server 是服务端的 KCP/UDP 地址。
	Server            string   `yaml:"server"`
	HeartbeatInterval Duration `yaml:"heartbeat_interval"`
	HeartbeatMiss     int      `yaml:"heartbeat_miss"`
	AuthTimeout       Duration `yaml:"auth_timeout"`
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
	Size           int      `yaml:"size"`
	MaxSessions    int      `yaml:"max_sessions"`
	IdleTimeout    Duration `yaml:"idle_timeout"`
	StartupJitter  Duration `yaml:"startup_jitter"`
	ConnectTimeout Duration `yaml:"connect_timeout"`
	BackoffMin     Duration `yaml:"backoff_min"`
	BackoffMax     Duration `yaml:"backoff_max"`
}

// Server 是 server: 段（只放服务端独有的参数）。
type Server struct {
	Listen        string     `yaml:"listen"`
	Auth          ServerAuth `yaml:"auth"`
	Dial          Dial       `yaml:"dial"`
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

// Log 是日志设置（两端可各自设置级别）。
type Log struct {
	Level string `yaml:"level"`
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

// ValidateClient 校验 common 段与 client 段（客户端启动时调用）。
func (c *Config) ValidateClient() error {
	return errors.Join(c.Common.Validate(), c.Client.Validate())
}

// ValidateServer 校验 common 段与 server 段（服务端启动时调用）。
func (c *Config) ValidateServer() error {
	return errors.Join(c.Common.Validate(), c.Server.Validate())
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

// Validate 校验 common: 段。
func (c *Common) Validate() error {
	p := &problems{}
	decodePSK(p, "common.psk", c.PSK)
	validateCrypt(p, "common.crypt", c.Crypt)
	validateAEAD(p, "common.aead", c.AEAD)
	validateKCPTuning(p, "common.kcp", &c.KCP)

	p.require(c.Stream.IdleTimeout.D() > 0, "common.stream.idle_timeout: 必须大于 0")

	p.require(c.Limits.MaxStreamsPerSession >= 1 && c.Limits.MaxStreamsPerSession <= MaxStreamsPerSess,
		"common.limits.max_streams_per_session: 需在 [1,%d]，实际 %d", MaxStreamsPerSess, c.Limits.MaxStreamsPerSession)
	p.require(c.Limits.MaxFrameBody > 0 && c.Limits.MaxFrameBody <= protocol.MaxFrameBody,
		"common.limits.max_frame_body: 需在 (0,%d]，实际 %d", protocol.MaxFrameBody, c.Limits.MaxFrameBody)
	minBody := protocol.BodyPrefixSize + protocol.TagSize
	p.require(c.Limits.MaxFrameBody > minBody,
		"common.limits.max_frame_body: 必须大于 %d，实际 %d", minBody, c.Limits.MaxFrameBody)
	p.require(c.Limits.MaxDataPayload >= 1, "common.limits.max_data_payload: 必须不小于 1")
	p.require(c.Limits.MaxDataPayload <= c.Limits.MaxFrameBody-minBody,
		"common.limits.max_data_payload: 不能超过 max_frame_body-%d（%d > %d）",
		minBody, c.Limits.MaxDataPayload, c.Limits.MaxFrameBody-minBody)
	return p.err()
}

// Validate 校验 client: 段。
func (c *Client) Validate() error {
	p := &problems{}
	validateAddr(p, "client.listen", c.Listen, false)
	validateAddr(p, "client.kcp.server", c.KCP.Server, true)

	p.require(c.Auth.Mode == LocalAuthNone || c.Auth.Mode == LocalAuthUserPass,
		"client.auth.mode: 只能是 %q 或 %q，实际 %q", LocalAuthNone, LocalAuthUserPass, c.Auth.Mode)
	if c.Auth.Mode == LocalAuthUserPass {
		p.add("client.auth.mode: userpass 认证尚未实现（见 dev.md §16.1）")
	}
	p.require(len(c.Auth.Users) == 0, "client.auth.users: userpass 尚未实现，必须为空")

	p.require(c.Socks5.HandshakeTimeout.D() > 0, "client.socks5.handshake_timeout: 必须大于 0")
	p.require(c.Pool.Size >= MinPoolSize && c.Pool.Size <= MaxPoolSize,
		"client.pool.size: 需在 [%d,%d]，实际 %d", MinPoolSize, MaxPoolSize, c.Pool.Size)
	p.require(c.Pool.MaxSessions >= MinPoolSize && c.Pool.MaxSessions <= MaxPoolSessions,
		"client.pool.max_sessions: 需在 [%d,%d]，实际 %d", MinPoolSize, MaxPoolSessions, c.Pool.MaxSessions)
	p.require(c.Pool.MaxSessions >= c.Pool.Size,
		"client.pool.max_sessions: 必须不小于 pool.size（%d < %d）", c.Pool.MaxSessions, c.Pool.Size)
	p.require(c.Pool.IdleTimeout.D() > 0, "client.pool.idle_timeout: 必须大于 0")
	p.require(c.Pool.StartupJitter.D() >= 0, "client.pool.startup_jitter: 不能为负")
	p.require(c.Pool.ConnectTimeout.D() > 0, "client.pool.connect_timeout: 必须大于 0")
	p.require(c.Pool.BackoffMin.D() > 0, "client.pool.backoff_min: 必须大于 0")
	p.require(c.Pool.BackoffMax.D() >= c.Pool.BackoffMin.D(),
		"client.pool.backoff_max: 不能小于 backoff_min")

	p.require(c.KCP.HeartbeatInterval.D() > 0, "client.kcp.heartbeat_interval: 必须大于 0")
	p.require(c.KCP.HeartbeatMiss >= 1, "client.kcp.heartbeat_miss: 必须不小于 1")
	p.require(c.KCP.AuthTimeout.D() > 0, "client.kcp.auth_timeout: 必须大于 0")

	p.require(c.ShutdownGrace.D() > 0, "client.shutdown_grace: 必须大于 0")
	validateLevel(p, "client.log.level", c.Log.Level)
	return p.err()
}

// Validate 校验 server: 段。
func (s *Server) Validate() error {
	p := &problems{}
	validateAddr(p, "server.listen", s.Listen, false)

	p.require(s.Auth.TimestampWindow.D() > 0, "server.auth.timestamp_window: 必须大于 0")
	p.require(s.Auth.AuthTimeout.D() > 0, "server.auth.auth_timeout: 必须大于 0")
	p.require(s.Auth.ReplayCacheSize > 0, "server.auth.replay_cache_size: 必须大于 0")
	p.require(s.Auth.MaxPendingSessions > 0, "server.auth.max_pending_sessions: 必须大于 0")

	p.require(s.Dial.Timeout.D() > 0, "server.dial.timeout: 必须大于 0")
	p.require(s.Dial.Keepalive.D() > 0, "server.dial.keepalive: 必须大于 0")

	p.require(s.ShutdownGrace.D() > 0, "server.shutdown_grace: 必须大于 0")
	validateLevel(p, "server.log.level", s.Log.Level)
	return p.err()
}

// PSKBytes 返回解码后的 PSK。
func (c *Common) PSKBytes() ([]byte, error) {
	psk, err := decodeBase64PSK(c.PSK)
	if err != nil {
		return nil, fmt.Errorf("common.psk: %w", err)
	}
	return psk, nil
}

// CryptName 返回规范化后的传输层加密算法名（空值即 none）。
func (c *Common) CryptName() string { return normalizeCrypt(c.Crypt) }

// AEADName 返回规范化后的应用层 AEAD 算法名。
func (c *Common) AEADName() string { return normalizeAEAD(c.AEAD) }

// LevelOrDefault 返回规范化后的日志级别（空值即 info）。
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
