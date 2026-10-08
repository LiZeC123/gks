package protocol

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
)

// SOCKS5 回复码（REP）。CONNECT_RESP 与本地 SOCKS5 回复共用同一套码，
// 这样服务端的拨号结果可以原样透传给本地客户端（dev.md §5.3）。
const (
	RepSuccess              byte = 0x00
	RepGeneralFailure       byte = 0x01
	RepNotAllowed           byte = 0x02
	RepNetworkUnreachable   byte = 0x03
	RepHostUnreachable      byte = 0x04
	RepConnectionRefused    byte = 0x05
	RepTTLExpired           byte = 0x06
	RepCommandNotSupported  byte = 0x07
	RepAddrTypeNotSupported byte = 0x08
)

// RepString 返回回复码的可读名（用于日志）。
func RepString(code byte) string {
	switch code {
	case RepSuccess:
		return "SUCCESS"
	case RepGeneralFailure:
		return "GENERAL_FAILURE"
	case RepNotAllowed:
		return "NOT_ALLOWED"
	case RepNetworkUnreachable:
		return "NETWORK_UNREACHABLE"
	case RepHostUnreachable:
		return "HOST_UNREACHABLE"
	case RepConnectionRefused:
		return "CONNECTION_REFUSED"
	case RepTTLExpired:
		return "TTL_EXPIRED"
	case RepCommandNotSupported:
		return "COMMAND_NOT_SUPPORTED"
	case RepAddrTypeNotSupported:
		return "ADDR_TYPE_NOT_SUPPORTED"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02X)", code)
	}
}

// ReplyFromDialError 把拨号错误映射为 SOCKS5 回复码（dev.md §5.3）。
func ReplyFromDialError(err error) byte {
	if err == nil {
		return RepSuccess
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return RepHostUnreachable
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return RepTTLExpired
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, syscall.ETIMEDOUT):
		return RepTTLExpired
	case errors.Is(err, syscall.ECONNREFUSED):
		return RepConnectionRefused
	case errors.Is(err, syscall.ENETUNREACH):
		return RepNetworkUnreachable
	case errors.Is(err, syscall.EHOSTUNREACH):
		return RepHostUnreachable
	default:
		return RepGeneralFailure
	}
}

// ConnectRequest 是 CONNECT_REQ 的 Payload：目标地址（无额外字段）。
type ConnectRequest struct {
	Address Address
}

// Marshal 编码为 CONNECT_REQ payload。
func (r ConnectRequest) Marshal() ([]byte, error) { return r.Address.Marshal() }

// ParseConnectRequest 解析 CONNECT_REQ payload，且必须完全消耗输入。
func ParseConnectRequest(b []byte) (ConnectRequest, error) {
	addr, n, err := ParseAddress(b)
	if err != nil {
		return ConnectRequest{}, err
	}
	if n != len(b) {
		return ConnectRequest{}, fmt.Errorf("%w: CONNECT_REQ 尾部有 %d 字节多余数据", ErrBadAddress, len(b)-n)
	}
	return ConnectRequest{Address: addr}, nil
}

// ConnectResponse 是 CONNECT_RESP 的 Payload：回复码 + BND 地址。
type ConnectResponse struct {
	Reply byte
	Bind  Address
}

// Marshal 编码为 CONNECT_RESP payload。
func (r ConnectResponse) Marshal() ([]byte, error) {
	bind, err := r.Bind.Marshal()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(bind))
	out = append(out, r.Reply)
	return append(out, bind...), nil
}

// ParseConnectResponse 解析 CONNECT_RESP payload。
func ParseConnectResponse(b []byte) (ConnectResponse, error) {
	if len(b) < 2 {
		return ConnectResponse{}, fmt.Errorf("%w: CONNECT_RESP 至少需要 2 字节，实际 %d", ErrBadAddress, len(b))
	}
	addr, n, err := ParseAddress(b[1:])
	if err != nil {
		return ConnectResponse{}, err
	}
	if 1+n != len(b) {
		return ConnectResponse{}, fmt.Errorf("%w: CONNECT_RESP 尾部有 %d 字节多余数据", ErrBadAddress, len(b)-1-n)
	}
	return ConnectResponse{Reply: b[0], Bind: addr}, nil
}

// UnspecificBind 返回占位用的 BND 地址（0.0.0.0:0）。
//
// 本地 SOCKS5 回复与 CONNECT_RESP 都可以用它；多数客户端不会使用 BND。
func UnspecificBind() Address {
	return Address{ATYP: ATYPIPv4, Host: "0.0.0.0", Port: 0}
}
