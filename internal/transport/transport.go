// Package transport 封装 KCP 会话的建立（拨号与监听）。
//
// 传输层加密（dev.md §0.3 阶段 A）通过 Options.Crypt 开关：默认 none，
// 即 KCP 头在线上明文；开启后由 kcp-go 的 crypt 覆盖 FEC 头、KCP 头与载荷。
// 注意 kcp-go 的 crypt 只解决「传输层机密性与头篡改」，不提供身份认证与防重放，
// 因此应用层的 AUTH 与 AEAD 无论如何都必须保留。
package transport

import (
	"fmt"
	"net"
	"time"

	kcp "github.com/xtaci/kcp-go/v5"

	"github.com/LiZeC123/gks/internal/protocol"
)

// Options 是传输层参数（来自配置的 kcp 段）。
type Options struct {
	Interval     time.Duration
	MTU          int
	SndWnd       int
	RcvWnd       int
	DataShards   int
	ParityShards int
	// Crypt ∈ {none, aes-128-gcm, aes-256-gcm, aes-128, aes-256, salsa20}，空值即 none。
	Crypt string
	// PSK 用于派生传输层密钥（仅在 Crypt != none 时使用）。
	PSK []byte
}

// blockCrypt 按配置构造 kcp-go 的加密器；none 时返回 nil（kcp-go 据此跳过加解密）。
func blockCrypt(opts Options) (kcp.BlockCrypt, error) {
	name := opts.Crypt
	if name == "" {
		name = "none"
	}
	if name == "none" {
		return nil, nil
	}

	keyLen := protocol.AEADKeySize
	switch name {
	case "aes-128-gcm", "aes-128":
		keyLen = 16
	case "aes-256-gcm", "aes-256", "salsa20":
		keyLen = protocol.AEADKeySize
	default:
		return nil, fmt.Errorf("transport: 不支持的 crypt %q", opts.Crypt)
	}

	// 传输层密钥必须独立派生，不得直接截断 PSK。
	info := fmt.Sprintf("gks-kcp-%s-v1", name)
	key, err := protocol.DeriveKey(opts.PSK, nil, info, keyLen)
	if err != nil {
		return nil, fmt.Errorf("transport: 派生 %s 密钥: %w", name, err)
	}

	switch name {
	case "aes-128", "aes-256":
		return kcp.NewAESBlockCrypt(key)
	case "salsa20":
		return kcp.NewSalsa20BlockCrypt(key)
	default: // aes-128-gcm / aes-256-gcm
		return kcp.NewAESGCMCrypt(key)
	}
}

func intervalMS(d time.Duration) int {
	ms := int(d / time.Millisecond)
	if ms < 1 {
		ms = 10
	}
	return ms
}

// applyTuning 把 dev.md §6 的 KCP 参数应用到会话上。
func applyTuning(s *kcp.UDPSession, o Options) error {
	s.SetNoDelay(1, intervalMS(o.Interval), 2, 1)
	s.SetWindowSize(o.SndWnd, o.RcvWnd)
	s.SetStreamMode(true)
	if o.MTU > 0 && !s.SetMtu(o.MTU) {
		return fmt.Errorf("transport: MTU %d 不受支持", o.MTU)
	}
	return nil
}

// Listener 是 Server 侧的 KCP 监听器。
type Listener struct {
	l    *kcp.Listener
	opts Options
}

// Listen 在 addr 上监听 KCP/UDP。
func Listen(addr string, opts Options) (*Listener, error) {
	block, err := blockCrypt(opts)
	if err != nil {
		return nil, err
	}
	l, err := kcp.ListenWithOptions(addr, block, opts.DataShards, opts.ParityShards)
	if err != nil {
		return nil, fmt.Errorf("transport: 监听 %s: %w", addr, err)
	}
	return &Listener{l: l, opts: opts}, nil
}

// Accept 接受一条新的 KCP 会话。
func (l *Listener) Accept() (net.Conn, error) {
	sess, err := l.l.AcceptKCP()
	if err != nil {
		return nil, err
	}
	if err := applyTuning(sess, l.opts); err != nil {
		_ = sess.Close()
		return nil, err
	}
	return sess, nil
}

// Addr 返回实际监听地址（端口为 0 时可用于取回随机端口）。
func (l *Listener) Addr() net.Addr { return l.l.Addr() }

// Close 关闭监听器。
func (l *Listener) Close() error { return l.l.Close() }

// Dialer 是 Client 侧的 KCP 拨号器。
type Dialer struct {
	opts Options
}

// NewDialer 构造拨号器。
func NewDialer(opts Options) *Dialer { return &Dialer{opts: opts} }

// Dial 建立一条 KCP 会话。
func (d *Dialer) Dial(addr string) (net.Conn, error) {
	block, err := blockCrypt(d.opts)
	if err != nil {
		return nil, err
	}
	sess, err := kcp.DialWithOptions(addr, block, d.opts.DataShards, d.opts.ParityShards)
	if err != nil {
		return nil, fmt.Errorf("transport: 连接 %s: %w", addr, err)
	}
	if err := applyTuning(sess, d.opts); err != nil {
		_ = sess.Close()
		return nil, err
	}
	return sess, nil
}
