package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
)

// SOCKS5 地址类型（ATYP）。CONNECT_REQ/CONNECT_RESP 复用同一套线格式，
// 因此这里定义的常量同时服务于 SOCKS5 报文与 gks 帧。
const (
	ATYPIPv4   byte = 0x01
	ATYPDomain byte = 0x03
	ATYPIPv6   byte = 0x04
)

// 地址编解码错误。
var (
	ErrBadAddress  = errors.New("protocol: 地址格式非法")
	ErrBadATYP     = errors.New("protocol: 不支持的地址类型")
	ErrEmptyHost   = errors.New("protocol: 主机名为空")
	ErrHostTooLong = errors.New("protocol: 域名超过 255 字节")
)

// MaxDomainLen 是域名长度上限（线格式用 1 字节表示长度）。
const MaxDomainLen = 255

// Address 是一个目标地址，线格式与 SOCKS5 一致：ATYP(1B) + ADDR + PORT(2B, BE)。
type Address struct {
	ATYP byte
	Host string
	Port uint16
}

// Address 返回 "host:port" 形式（IPv6 会带方括号），可直接用于 net.Dial。
func (a Address) Address() string {
	return net.JoinHostPort(a.Host, strconv.Itoa(int(a.Port)))
}

// String 返回便于日志阅读的形式。
func (a Address) String() string { return a.Address() }

// Marshal 编码为线格式。
func (a Address) Marshal() ([]byte, error) {
	switch a.ATYP {
	case ATYPIPv4:
		ip := net.ParseIP(a.Host)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("%w: %q 不是 IPv4", ErrBadAddress, a.Host)
		}
		out := make([]byte, 0, 1+net.IPv4len+2)
		out = append(out, ATYPIPv4)
		out = append(out, ip.To4()...)
		return binary.BigEndian.AppendUint16(out, a.Port), nil
	case ATYPIPv6:
		ip := net.ParseIP(a.Host)
		if ip == nil || ip.To4() != nil || ip.To16() == nil {
			return nil, fmt.Errorf("%w: %q 不是 IPv6", ErrBadAddress, a.Host)
		}
		out := make([]byte, 0, 1+net.IPv6len+2)
		out = append(out, ATYPIPv6)
		out = append(out, ip.To16()...)
		return binary.BigEndian.AppendUint16(out, a.Port), nil
	case ATYPDomain:
		if a.Host == "" {
			return nil, ErrEmptyHost
		}
		if len(a.Host) > MaxDomainLen {
			return nil, fmt.Errorf("%w: %d", ErrHostTooLong, len(a.Host))
		}
		out := make([]byte, 0, 2+len(a.Host)+2)
		out = append(out, ATYPDomain, byte(len(a.Host)))
		out = append(out, a.Host...)
		return binary.BigEndian.AppendUint16(out, a.Port), nil
	default:
		return nil, fmt.Errorf("%w: 0x%02X", ErrBadATYP, a.ATYP)
	}
}

// ParseAddress 解析线格式地址，返回地址与消耗的字节数。
func ParseAddress(b []byte) (Address, int, error) {
	if len(b) < 1 {
		return Address{}, 0, fmt.Errorf("%w: 至少需要 1 字节", ErrBadAddress)
	}
	switch b[0] {
	case ATYPIPv4:
		const n = 1 + net.IPv4len + 2
		if len(b) < n {
			return Address{}, 0, fmt.Errorf("%w: IPv4 需要 %d 字节，实际 %d", ErrBadAddress, n, len(b))
		}
		host := net.IP(b[1 : 1+net.IPv4len]).String()
		return Address{ATYP: ATYPIPv4, Host: host, Port: binary.BigEndian.Uint16(b[1+net.IPv4len : n])}, n, nil
	case ATYPIPv6:
		const n = 1 + net.IPv6len + 2
		if len(b) < n {
			return Address{}, 0, fmt.Errorf("%w: IPv6 需要 %d 字节，实际 %d", ErrBadAddress, n, len(b))
		}
		host := net.IP(b[1 : 1+net.IPv6len]).String()
		return Address{ATYP: ATYPIPv6, Host: host, Port: binary.BigEndian.Uint16(b[1+net.IPv6len : n])}, n, nil
	case ATYPDomain:
		if len(b) < 2 {
			return Address{}, 0, fmt.Errorf("%w: 域名缺少长度字段", ErrBadAddress)
		}
		n := int(b[1])
		if n == 0 {
			return Address{}, 0, ErrEmptyHost
		}
		total := 2 + n + 2
		if len(b) < total {
			return Address{}, 0, fmt.Errorf("%w: 域名需要 %d 字节，实际 %d", ErrBadAddress, total, len(b))
		}
		host := string(b[2 : 2+n])
		return Address{ATYP: ATYPDomain, Host: host, Port: binary.BigEndian.Uint16(b[2+n : total])}, total, nil
	default:
		return Address{}, 0, fmt.Errorf("%w: 0x%02X", ErrBadATYP, b[0])
	}
}

// AddressFromHostPort 按主机串形态推断 ATYP 并构造 Address。
//
// 域名（含 localhost 这类名字）用 ATYPDomain，由服务端解析；
// 这样域名解析发生在服务端，避免本地 DNS 泄漏（dev.md §0.2）。
func AddressFromHostPort(host string, port uint16) (Address, error) {
	if host == "" {
		return Address{}, ErrEmptyHost
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() != nil {
			return Address{ATYP: ATYPIPv4, Host: ip.To4().String(), Port: port}, nil
		}
		return Address{ATYP: ATYPIPv6, Host: ip.String(), Port: port}, nil
	}
	if len(host) > MaxDomainLen {
		return Address{}, fmt.Errorf("%w: %d", ErrHostTooLong, len(host))
	}
	return Address{ATYP: ATYPDomain, Host: host, Port: port}, nil
}
