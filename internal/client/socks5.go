// Package client 实现 gks 客户端：本地 SOCKS5 服务 + 到服务端的 KCP 会话。
package client

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/LiZeC123/gks/internal/protocol"
)

// SOCKS5 常量（RFC 1928 / RFC 1929）。
const (
	socksVersion = 0x05

	cmdConnect  = 0x01
	cmdBind     = 0x02
	cmdUDPAssoc = 0x03

	methodNoAuth       = 0x00
	methodUserPass     = 0x02
	methodNoAcceptable = 0xFF
)

// SOCKS5 协议错误。
var (
	ErrBadVersion         = errors.New("socks5: 版本号不是 5")
	ErrNoAcceptableMethod = errors.New("socks5: 客户端未提供无认证方式")
	ErrBadReserved        = errors.New("socks5: RSV 必须为 0")
	ErrCmdNotSupported    = errors.New("socks5: 只支持 CONNECT")
	ErrShortRequest       = errors.New("socks5: 请求报文不完整")
)

// Target 是 SOCKS5 请求里的目标地址（与 gks 的 CONNECT_REQ 复用同一线格式）。
type Target = protocol.Address

// Negotiate 完成 SOCKS5 方法协商：仅支持「无认证」。
//
// 客户端没有提供 0x00 时回复 0xFF 并返回 ErrNoAcceptableMethod。
func Negotiate(rw io.ReadWriter) error {
	var head [2]byte
	if _, err := io.ReadFull(rw, head[:]); err != nil {
		return fmt.Errorf("socks5: 读取握手头: %w", err)
	}
	if head[0] != socksVersion {
		return fmt.Errorf("%w: 0x%02X", ErrBadVersion, head[0])
	}
	n := int(head[1])
	if n == 0 {
		return ErrNoAcceptableMethod
	}
	methods := make([]byte, n)
	if _, err := io.ReadFull(rw, methods); err != nil {
		return fmt.Errorf("socks5: 读取方法列表: %w", err)
	}
	for _, m := range methods {
		if m == methodNoAuth {
			if _, err := rw.Write([]byte{socksVersion, methodNoAuth}); err != nil {
				return fmt.Errorf("socks5: 回复方法选择: %w", err)
			}
			return nil
		}
	}
	_, _ = rw.Write([]byte{socksVersion, methodNoAcceptable})
	return ErrNoAcceptableMethod
}

// ReadRequest 读取 SOCKS5 请求并返回目标地址。
//
// 仅接受 CMD=CONNECT；UDP ASSOCIATE / BIND 返回 ErrCmdNotSupported，
// 不支持的地址类型返回 protocol.ErrBadATYP（调用方据此回 0x07 / 0x08）。
func ReadRequest(r io.Reader) (Target, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Target{}, fmt.Errorf("%w: %w", ErrShortRequest, err)
	}
	if head[0] != socksVersion {
		return Target{}, fmt.Errorf("%w: 0x%02X", ErrBadVersion, head[0])
	}
	if head[2] != 0x00 {
		return Target{}, fmt.Errorf("%w: 0x%02X", ErrBadReserved, head[2])
	}
	if head[1] != cmdConnect {
		return Target{}, fmt.Errorf("%w: CMD=0x%02X", ErrCmdNotSupported, head[1])
	}
	return readAddress(r, head[3])
}

func readAddress(r io.Reader, atyp byte) (Target, error) {
	switch atyp {
	case protocol.ATYPIPv4:
		buf := make([]byte, net.IPv4len+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Target{}, fmt.Errorf("%w: %w", ErrShortRequest, err)
		}
		return parseAddress(append([]byte{atyp}, buf...))
	case protocol.ATYPIPv6:
		buf := make([]byte, net.IPv6len+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Target{}, fmt.Errorf("%w: %w", ErrShortRequest, err)
		}
		return parseAddress(append([]byte{atyp}, buf...))
	case protocol.ATYPDomain:
		var l [1]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return Target{}, fmt.Errorf("%w: %w", ErrShortRequest, err)
		}
		if l[0] == 0 {
			return Target{}, protocol.ErrEmptyHost
		}
		buf := make([]byte, int(l[0])+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Target{}, fmt.Errorf("%w: %w", ErrShortRequest, err)
		}
		return parseAddress(append([]byte{atyp, l[0]}, buf...))
	default:
		return Target{}, fmt.Errorf("%w: 0x%02X", protocol.ErrBadATYP, atyp)
	}
}

func parseAddress(raw []byte) (Target, error) {
	addr, n, err := protocol.ParseAddress(raw)
	if err != nil {
		return Target{}, err
	}
	if n != len(raw) {
		return Target{}, fmt.Errorf("%w: 地址尾部有多余字节", protocol.ErrBadAddress)
	}
	return addr, nil
}

// WriteReply 写出 SOCKS5 回复：VER REP RSV ATYP BND.ADDR BND.PORT。
func WriteReply(w io.Writer, rep byte, bind Target) error {
	body, err := bind.Marshal()
	if err != nil {
		// BND 不可编码时退回占位地址，保证客户端能收到明确的失败码。
		body, err = protocol.UnspecificBind().Marshal()
		if err != nil {
			return err
		}
	}
	out := make([]byte, 0, 3+len(body))
	out = append(out, socksVersion, rep, 0x00)
	out = append(out, body...)
	_, err = w.Write(out)
	return err
}

// ReplyForError 把本地 SOCKS5 阶段的错误映射为回复码。
//
// 第二个返回值为 false 表示「不该再回 SOCKS5 回复」：协议版本不对、
// 方法协商已被拒（已回过 0xFF）或报文残缺时，直接断开更合适。
func ReplyForError(err error) (byte, bool) {
	switch {
	case err == nil:
		return protocol.RepSuccess, true
	case errors.Is(err, ErrCmdNotSupported):
		return protocol.RepCommandNotSupported, true
	case errors.Is(err, protocol.ErrBadATYP):
		return protocol.RepAddrTypeNotSupported, true
	case errors.Is(err, ErrShortRequest):
		return protocol.RepGeneralFailure, false
	default:
		return protocol.RepGeneralFailure, false
	}
}
