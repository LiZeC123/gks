package protocol

import (
	"container/list"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// 认证常量（dev.md §3.4）。
const (
	// AuthNonceSize 是 AUTH 握手中双方各自提供的随机数长度。
	AuthNonceSize = 16
	// AuthProofSize 是 HMAC-SHA256 证明长度。
	AuthProofSize = 32
	// AuthLabel 是 HMAC 的域分离标签。
	AuthLabel = "gks-auth-v1"
	// DefaultTokenID 是本版唯一的 token 标识。
	DefaultTokenID uint16 = 0x0001
	// AuthRequestSize 是 AUTH_REQ Payload 长度：token(2) + ts(8) + nonce_c(16) + proof(32)。
	AuthRequestSize = 2 + 8 + AuthNonceSize + AuthProofSize
	// AuthResponseOKSize 是认证成功时 AUTH_RESP Payload 长度：code(1) + nonce_s(16)。
	AuthResponseOKSize = 1 + AuthNonceSize

	// AuthCodeOK 表示认证成功。
	AuthCodeOK byte = 0x00
	// AuthCodeBadToken 表示 token 不匹配或 proof 校验失败。
	AuthCodeBadToken byte = 0x01
	// AuthCodeTimestamp 表示时间戳超出允许窗口。
	AuthCodeTimestamp byte = 0x02
	// AuthCodeReplay 表示 nonce_c 已被使用过（重放）。
	AuthCodeReplay byte = 0x03
	// AuthCodeOther 表示其他失败原因。
	AuthCodeOther byte = 0x04
)

// 认证相关错误。
var (
	ErrAuthFailed     = errors.New("protocol: 认证失败")
	ErrAuthMalformed  = errors.New("protocol: 认证帧格式非法")
	ErrBadPSK         = errors.New("protocol: PSK 长度不合法")
	ErrBadTokenID     = errors.New("protocol: token id 不合法")
	ErrReplayCacheCfg = errors.New("protocol: 重放缓存参数不合法")
)

// AuthCodeString 返回错误码的可读名（用于日志）。
func AuthCodeString(code byte) string {
	switch code {
	case AuthCodeOK:
		return "OK"
	case AuthCodeBadToken:
		return "BAD_TOKEN"
	case AuthCodeTimestamp:
		return "TIMESTAMP_OUT_OF_WINDOW"
	case AuthCodeReplay:
		return "REPLAY"
	case AuthCodeOther:
		return "OTHER"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02X)", code)
	}
}

// AuthRequest 是 AUTH_REQ 的 Payload。
type AuthRequest struct {
	TokenID   uint16
	Timestamp uint64 // Unix 秒
	NonceC    [AuthNonceSize]byte
	Proof     [AuthProofSize]byte
}

// Marshal 编码为 58 字节。
func (r AuthRequest) Marshal() []byte {
	b := make([]byte, AuthRequestSize)
	binary.BigEndian.PutUint16(b[0:2], r.TokenID)
	binary.BigEndian.PutUint64(b[2:10], r.Timestamp)
	copy(b[10:10+AuthNonceSize], r.NonceC[:])
	copy(b[10+AuthNonceSize:], r.Proof[:])
	return b
}

// ParseAuthRequest 解析 AUTH_REQ Payload。
func ParseAuthRequest(b []byte) (AuthRequest, error) {
	if len(b) != AuthRequestSize {
		return AuthRequest{}, fmt.Errorf("%w: AUTH_REQ 需要 %d 字节，实际 %d", ErrAuthMalformed, AuthRequestSize, len(b))
	}
	var r AuthRequest
	r.TokenID = binary.BigEndian.Uint16(b[0:2])
	r.Timestamp = binary.BigEndian.Uint64(b[2:10])
	copy(r.NonceC[:], b[10:10+AuthNonceSize])
	copy(r.Proof[:], b[10+AuthNonceSize:])
	return r, nil
}

// AuthResponse 是 AUTH_RESP 的 Payload。
type AuthResponse struct {
	Code   byte
	NonceS [AuthNonceSize]byte
}

// Marshal 编码：成功为 17 字节（含 nonce_s），失败为 1 字节。
func (r AuthResponse) Marshal() []byte {
	if r.Code != AuthCodeOK {
		return []byte{r.Code}
	}
	b := make([]byte, AuthResponseOKSize)
	b[0] = r.Code
	copy(b[1:], r.NonceS[:])
	return b
}

// ParseAuthResponse 解析 AUTH_RESP Payload，并校验长度与 code 一致。
func ParseAuthResponse(b []byte) (AuthResponse, error) {
	if len(b) == 0 {
		return AuthResponse{}, fmt.Errorf("%w: AUTH_RESP 为空", ErrAuthMalformed)
	}
	resp := AuthResponse{Code: b[0]}
	if resp.Code == AuthCodeOK {
		if len(b) != AuthResponseOKSize {
			return AuthResponse{}, fmt.Errorf("%w: 成功响应需要 %d 字节，实际 %d", ErrAuthMalformed, AuthResponseOKSize, len(b))
		}
		copy(resp.NonceS[:], b[1:])
		return resp, nil
	}
	if len(b) != 1 {
		return AuthResponse{}, fmt.Errorf("%w: 失败响应应为 1 字节，实际 %d", ErrAuthMalformed, len(b))
	}
	return resp, nil
}

// ComputeProof 计算 HMAC-SHA256(PSK, "gks-auth-v1" || token_id || nonce_c || timestamp)。
//
// PSK 本身永不出现在线路上；proof 也不能用于为新的 nonce_c 伪造证明。
func ComputeProof(psk []byte, tokenID uint16, nonceC []byte, timestamp uint64) []byte {
	mac := hmac.New(sha256.New, psk)
	mac.Write([]byte(AuthLabel))
	var hdr [10]byte
	binary.BigEndian.PutUint16(hdr[0:2], tokenID)
	binary.BigEndian.PutUint64(hdr[2:10], timestamp)
	mac.Write(hdr[:2])
	mac.Write(nonceC)
	mac.Write(hdr[2:])
	return mac.Sum(nil)
}

// ReplayCache 记录近期已使用过的 (token_id, nonce_c)，用于防重放。
//
// TTL 统一，因此插入顺序即过期顺序，可从头顺序淘汰。
type ReplayCache struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	ll       *list.List
	entries  map[replayKey]*list.Element
}

type replayKey struct {
	tokenID uint16
	nonce   [AuthNonceSize]byte
}

type replayEntry struct {
	key     replayKey
	expires time.Time
}

// NewReplayCache 构造重放缓存。capacity<=0 或 ttl<=0 时返回错误。
func NewReplayCache(capacity int, ttl time.Duration) (*ReplayCache, error) {
	if capacity <= 0 || ttl <= 0 {
		return nil, fmt.Errorf("%w: capacity=%d ttl=%s", ErrReplayCacheCfg, capacity, ttl)
	}
	return &ReplayCache{
		capacity: capacity,
		ttl:      ttl,
		ll:       list.New(),
		entries:  make(map[replayKey]*list.Element, capacity),
	}, nil
}

// Seen 报告 (tokenID, nonce) 是否已经出现过；未出现过则登记并返回 false。
func (c *ReplayCache) Seen(tokenID uint16, nonce [AuthNonceSize]byte, now time.Time) bool {
	if c == nil || c.capacity <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.purge(now)
	key := replayKey{tokenID: tokenID, nonce: nonce}
	if _, ok := c.entries[key]; ok {
		return true
	}
	c.entries[key] = c.ll.PushBack(replayEntry{key: key, expires: now.Add(c.ttl)})
	for c.ll.Len() > c.capacity {
		front := c.ll.Front()
		if front == nil {
			break
		}
		c.remove(front)
	}
	return false
}

// Len 返回当前缓存条目数（用于测试与指标）。
func (c *ReplayCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *ReplayCache) purge(now time.Time) {
	for {
		front := c.ll.Front()
		if front == nil {
			return
		}
		if front.Value.(replayEntry).expires.After(now) {
			return
		}
		c.remove(front)
	}
}

func (c *ReplayCache) remove(el *list.Element) {
	c.ll.Remove(el)
	delete(c.entries, el.Value.(replayEntry).key)
}

// HandshakeConfig 是握手双方的公共配置。
type HandshakeConfig struct {
	PSK     []byte
	AEAD    string
	TokenID uint16
	// Now 与 Rand 可为空，分别默认 time.Now 与 crypto/rand.Reader，测试时可注入。
	Now  func() time.Time
	Rand io.Reader
}

func (c *HandshakeConfig) normalize() error {
	if len(c.PSK) != PSKSize {
		return fmt.Errorf("%w: 需要 %d 字节，实际 %d", ErrBadPSK, PSKSize, len(c.PSK))
	}
	if c.AEAD == "" {
		c.AEAD = AEADChaCha20Poly1305
	}
	if !ValidAEAD(c.AEAD) {
		return fmt.Errorf("%w: %q", ErrUnknownAEAD, c.AEAD)
	}
	if c.TokenID == 0 {
		c.TokenID = DefaultTokenID
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Rand == nil {
		c.Rand = rand.Reader
	}
	return nil
}

func readRandom(r io.Reader, n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, fmt.Errorf("protocol: 读取随机数失败: %w", err)
	}
	return b, nil
}

// Initiator 是认证发起方（Client 侧）。
type Initiator struct {
	cfg HandshakeConfig
}

// NewInitiator 构造发起方。
func NewInitiator(cfg HandshakeConfig) (*Initiator, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &Initiator{cfg: cfg}, nil
}

// AEAD 返回本次握手中使用的 AEAD 算法名。
func (i *Initiator) AEAD() string { return i.cfg.AEAD }

// NewRequest 生成 AUTH_REQ（含随机 nonce_c）。
func (i *Initiator) NewRequest() (AuthRequest, error) {
	var req AuthRequest
	req.TokenID = i.cfg.TokenID
	req.Timestamp = uint64(i.cfg.Now().Unix())
	nonceC, err := readRandom(i.cfg.Rand, AuthNonceSize)
	if err != nil {
		return AuthRequest{}, err
	}
	copy(req.NonceC[:], nonceC)
	copy(req.Proof[:], ComputeProof(i.cfg.PSK, req.TokenID, req.NonceC[:], req.Timestamp))
	return req, nil
}

// Complete 校验 AUTH_RESP 并派生会话密钥。req 必须是 NewRequest 的返回值。
func (i *Initiator) Complete(req AuthRequest, resp AuthResponse) (DirectionKeys, error) {
	if resp.Code != AuthCodeOK {
		return DirectionKeys{}, fmt.Errorf("%w: code=%s", ErrAuthFailed, AuthCodeString(resp.Code))
	}
	return NewSessionKeys(i.cfg.AEAD, i.cfg.PSK, req.NonceC[:], resp.NonceS[:])
}

// AuthenticatorConfig 是认证响应方（Server 侧）的配置。
type AuthenticatorConfig struct {
	HandshakeConfig
	// Window 是允许的时间戳偏差窗口。
	Window time.Duration
	// Replay 为 nil 时按 (DefaultReplayCacheSize, 2*Window) 自动创建。
	Replay *ReplayCache
	// ReplayCacheSize 仅在 Replay 为 nil 时使用。
	ReplayCacheSize int
}

// DefaultReplayCacheSize 是重放缓存默认容量。
const DefaultReplayCacheSize = 65536

// Authenticator 是认证响应方（Server 侧）。
type Authenticator struct {
	cfg    HandshakeConfig
	window time.Duration
	replay *ReplayCache
}

// NewAuthenticator 构造响应方。
func NewAuthenticator(cfg AuthenticatorConfig) (*Authenticator, error) {
	if err := cfg.HandshakeConfig.normalize(); err != nil {
		return nil, err
	}
	if cfg.Window <= 0 {
		return nil, fmt.Errorf("%w: window=%s", ErrReplayCacheCfg, cfg.Window)
	}
	replay := cfg.Replay
	if replay == nil {
		size := cfg.ReplayCacheSize
		if size <= 0 {
			size = DefaultReplayCacheSize
		}
		var err error
		replay, err = NewReplayCache(size, 2*cfg.Window)
		if err != nil {
			return nil, err
		}
	}
	return &Authenticator{cfg: cfg.HandshakeConfig, window: cfg.Window, replay: replay}, nil
}

// AEAD 返回本次握手中使用的 AEAD 算法名。
func (a *Authenticator) AEAD() string { return a.cfg.AEAD }

// Verify 校验 AUTH_REQ。
//
// 返回的 AuthResponse 一定可以直接回给对方（失败时 code 为具体错误码）；
// error 仅在内部故障（如随机数不可用）时非 nil。
//
// 校验顺序为 token → 时间窗 → HMAC proof → 重放缓存：
// 把重放检查放在 HMAC 之后，避免未认证的请求污染重放缓存（缓存投毒）。
func (a *Authenticator) Verify(req AuthRequest) (AuthResponse, error) {
	if req.TokenID != a.cfg.TokenID {
		return AuthResponse{Code: AuthCodeBadToken}, nil
	}
	if !a.withinWindow(req.Timestamp) {
		return AuthResponse{Code: AuthCodeTimestamp}, nil
	}
	want := ComputeProof(a.cfg.PSK, req.TokenID, req.NonceC[:], req.Timestamp)
	if !hmac.Equal(want, req.Proof[:]) {
		return AuthResponse{Code: AuthCodeBadToken}, nil
	}
	if a.replay.Seen(req.TokenID, req.NonceC, a.cfg.Now()) {
		return AuthResponse{Code: AuthCodeReplay}, nil
	}
	nonceS, err := readRandom(a.cfg.Rand, AuthNonceSize)
	if err != nil {
		return AuthResponse{}, err
	}
	resp := AuthResponse{Code: AuthCodeOK}
	copy(resp.NonceS[:], nonceS)
	return resp, nil
}

// Complete 在认证成功后派生会话密钥。
func (a *Authenticator) Complete(req AuthRequest, resp AuthResponse) (DirectionKeys, error) {
	if resp.Code != AuthCodeOK {
		return DirectionKeys{}, fmt.Errorf("%w: code=%s", ErrAuthFailed, AuthCodeString(resp.Code))
	}
	return NewSessionKeys(a.cfg.AEAD, a.cfg.PSK, req.NonceC[:], resp.NonceS[:])
}

// withinWindow 用无符号比较判断时间戳偏差，避免负数与溢出问题。
func (a *Authenticator) withinWindow(ts uint64) bool {
	now := uint64(a.cfg.Now().Unix())
	var skew uint64
	if ts > now {
		skew = ts - now
	} else {
		skew = now - ts
	}
	windowSec := uint64((a.window + time.Second - 1) / time.Second)
	return skew <= windowSec
}
