package protocol

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// AEAD 算法名（配置项 aead）。未指定时使用 AEADChaCha20Poly1305。
const (
	AEADChaCha20Poly1305 = "chacha20-poly1305"
	AEADAES256GCM        = "aes-256-gcm"
)

// 密钥与 nonce 规格。
const (
	// PSKSize 是预共享密钥长度（dev.md §3.4）。
	PSKSize = 32
	// AEADKeySize 是会话密钥长度（两种算法均为 32B）。
	AEADKeySize = 32
	// NonceSize 是 AEAD nonce 长度：4B 前缀 0 + 8B 大端计数器。
	NonceSize = 12
	// noncePrefixSize 是 nonce 中保留为 0 的前缀长度。
	noncePrefixSize = NonceSize - 8
)

// Cipher 就是标准库的 AEAD 接口（chacha20poly1305 与 AES-GCM 都满足）。
type Cipher = cipher.AEAD

// 密钥派生相关的错误。
var (
	ErrBadKeySize     = errors.New("protocol: 密钥长度不合法")
	ErrUnknownAEAD    = errors.New("protocol: 未知 AEAD 算法")
	ErrNonceExhausted = errors.New("protocol: nonce 计数器耗尽")
	ErrBadAuthNonce   = errors.New("protocol: 认证 nonce 长度不合法")
)

// ValidAEAD 报告 name 是否为受支持的 AEAD 算法。
func ValidAEAD(name string) bool {
	switch name {
	case AEADChaCha20Poly1305, AEADAES256GCM:
		return true
	default:
		return false
	}
}

// NewAEAD 按算法名与密钥构造 AEAD。key 必须为 32 字节。
func NewAEAD(name string, key []byte) (Cipher, error) {
	if len(key) != AEADKeySize {
		return nil, fmt.Errorf("%w: %s 需要 %d 字节，实际 %d", ErrBadKeySize, name, AEADKeySize, len(key))
	}
	switch name {
	case AEADChaCha20Poly1305:
		return chacha20poly1305.New(key)
	case AEADAES256GCM:
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("protocol: 构造 AES: %w", err)
		}
		return cipher.NewGCM(block)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownAEAD, name)
	}
}

// DeriveKey 用 HKDF-SHA256 从 psk 派生 length 字节密钥。
//
// info 用于域分离（例如 "gks-c2s-v1"、"gks-kcp-aes-128-gcm-v1"），
// 保证同一 PSK 在不同用途下得到互不相关的密钥。
func DeriveKey(psk, salt []byte, info string, length int) ([]byte, error) {
	if len(psk) == 0 {
		return nil, fmt.Errorf("%w: psk 为空", ErrBadKeySize)
	}
	if length <= 0 || length > 255*sha256.Size {
		return nil, fmt.Errorf("%w: 目标长度 %d", ErrBadKeySize, length)
	}
	out := make([]byte, length)
	r := hkdf.New(sha256.New, psk, salt, []byte(info))
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("protocol: HKDF 派生失败: %w", err)
	}
	return out, nil
}

// DirectionKeys 是一条会话的双向密钥。
//
// 双向独立密钥是消除 nonce 冲突的根本手段（dev.md §3.5）：
// 每个方向各有自己的密钥与自己的 nonce 计数器，都从 0 开始也不会碰撞。
type DirectionKeys struct {
	C2S Cipher
	S2C Cipher
}

// NewSessionKeys 由 PSK 与双方 nonce 派生双向会话密钥。
func NewSessionKeys(aeadName string, psk, nonceC, nonceS []byte) (DirectionKeys, error) {
	if len(psk) != PSKSize {
		return DirectionKeys{}, fmt.Errorf("%w: psk=%d", ErrBadKeySize, len(psk))
	}
	if len(nonceC) != AuthNonceSize || len(nonceS) != AuthNonceSize {
		return DirectionKeys{}, fmt.Errorf("%w: nonce_c=%d nonce_s=%d", ErrBadAuthNonce, len(nonceC), len(nonceS))
	}
	salt := make([]byte, 0, len(nonceC)+len(nonceS))
	salt = append(salt, nonceC...)
	salt = append(salt, nonceS...)

	kc, err := DeriveKey(psk, salt, "gks-c2s-v1", AEADKeySize)
	if err != nil {
		return DirectionKeys{}, err
	}
	ks, err := DeriveKey(psk, salt, "gks-s2c-v1", AEADKeySize)
	if err != nil {
		return DirectionKeys{}, err
	}
	c2s, err := NewAEAD(aeadName, kc)
	if err != nil {
		return DirectionKeys{}, err
	}
	s2c, err := NewAEAD(aeadName, ks)
	if err != nil {
		return DirectionKeys{}, err
	}
	return DirectionKeys{C2S: c2s, S2C: s2c}, nil
}

// NonceCounter 生成 12 字节 nonce：4 字节 0 前缀 + 8 字节大端单调计数器。
//
// 每条方向各持有一个，从 0 开始。计数器不复用、不回绕：
// 取到 2^64-1 这个值时即报错（宁可断开也不重复使用 nonce）。
type NonceCounter struct {
	mu   sync.Mutex
	next uint64
}

// NewNonceCounter 返回从 0 开始的计数器。
func NewNonceCounter() *NonceCounter { return &NonceCounter{} }

// Next 返回下一个 nonce 的副本。
func (n *NonceCounter) Next() ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.next == math.MaxUint64 {
		return nil, ErrNonceExhausted
	}
	nonce := make([]byte, NonceSize)
	binary.BigEndian.PutUint64(nonce[noncePrefixSize:], n.next)
	n.next++
	return nonce, nil
}

// Count 返回已经发出的 nonce 个数（用于日志与测试）。
func (n *NonceCounter) Count() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.next
}

// RandomBytes 返回 n 字节密码学随机数据。
func RandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("protocol: 读取随机数失败: %w", err)
	}
	return b, nil
}
