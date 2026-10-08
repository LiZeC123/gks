package protocol

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestConnectRequestRoundTrip(t *testing.T) {
	cases := []Address{
		{ATYP: ATYPIPv4, Host: "127.0.0.1", Port: 2080},
		{ATYP: ATYPDomain, Host: "www.baidu.com", Port: 80},
		{ATYP: ATYPIPv6, Host: "::1", Port: 8080},
	}
	for _, addr := range cases {
		raw, err := ConnectRequest{Address: addr}.Marshal()
		if err != nil {
			t.Fatalf("Marshal(%v): %v", addr, err)
		}
		got, err := ParseConnectRequest(raw)
		if err != nil {
			t.Fatalf("ParseConnectRequest(%v): %v", addr, err)
		}
		if got.Address != addr {
			t.Fatalf("round-trip = %+v，期望 %+v", got.Address, addr)
		}
	}
}

func TestConnectRequestRejectsTrailingGarbage(t *testing.T) {
	raw, err := ConnectRequest{Address: Address{ATYP: ATYPIPv4, Host: "1.2.3.4", Port: 80}}.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := ParseConnectRequest(append(raw, 0xFF)); !errors.Is(err, ErrBadAddress) {
		t.Fatalf("err = %v，期望 ErrBadAddress", err)
	}
	if _, err := ParseConnectRequest(nil); !errors.Is(err, ErrBadAddress) {
		t.Fatalf("空输入 err = %v", err)
	}
}

func TestConnectResponseRoundTrip(t *testing.T) {
	cases := []ConnectResponse{
		{Reply: RepSuccess, Bind: Address{ATYP: ATYPIPv4, Host: "10.0.0.1", Port: 40000}},
		{Reply: RepConnectionRefused, Bind: UnspecificBind()},
		{Reply: RepHostUnreachable, Bind: Address{ATYP: ATYPDomain, Host: "example.com", Port: 443}},
	}
	for _, want := range cases {
		raw, err := want.Marshal()
		if err != nil {
			t.Fatalf("Marshal(%+v): %v", want, err)
		}
		got, err := ParseConnectResponse(raw)
		if err != nil {
			t.Fatalf("ParseConnectResponse(%+v): %v", want, err)
		}
		if got != want {
			t.Fatalf("round-trip = %+v，期望 %+v", got, want)
		}
	}
}

func TestConnectResponseErrors(t *testing.T) {
	if _, err := ParseConnectResponse(nil); !errors.Is(err, ErrBadAddress) {
		t.Fatalf("空输入: %v", err)
	}
	if _, err := ParseConnectResponse([]byte{RepSuccess}); !errors.Is(err, ErrBadAddress) {
		t.Fatalf("只有 1 字节: %v", err)
	}
	raw, err := ConnectResponse{Reply: RepSuccess, Bind: UnspecificBind()}.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := ParseConnectResponse(append(raw, 0x00)); !errors.Is(err, ErrBadAddress) {
		t.Fatalf("尾部多余数据: %v", err)
	}
	// Marshal 失败路径：BND 地址非法。
	if _, err := (ConnectResponse{Reply: RepSuccess, Bind: Address{ATYP: 0x09}}).Marshal(); !errors.Is(err, ErrBadATYP) {
		t.Fatalf("非法 BND: %v", err)
	}
}

func TestReplyFromDialError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want byte
	}{
		{"成功", nil, RepSuccess},
		{"连接被拒", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, RepConnectionRefused},
		{"网络不可达", &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}, RepNetworkUnreachable},
		{"主机不可达", &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}, RepHostUnreachable},
		{"超时", &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}, RepTTLExpired},
		{"context 超时", context.DeadlineExceeded, RepTTLExpired},
		{"系统调用超时", &net.OpError{Op: "dial", Err: syscall.ETIMEDOUT}, RepTTLExpired},
		{"DNS 失败", &net.DNSError{Err: "no such host", Name: "nope.invalid"}, RepHostUnreachable},
		{"其它错误", errors.New("boom"), RepGeneralFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReplyFromDialError(tc.err); got != tc.want {
				t.Fatalf("ReplyFromDialError = %s，期望 %s", RepString(got), RepString(tc.want))
			}
		})
	}
}

func TestRepString(t *testing.T) {
	cases := map[byte]string{
		RepSuccess:              "SUCCESS",
		RepGeneralFailure:       "GENERAL_FAILURE",
		RepNotAllowed:           "NOT_ALLOWED",
		RepNetworkUnreachable:   "NETWORK_UNREACHABLE",
		RepHostUnreachable:      "HOST_UNREACHABLE",
		RepConnectionRefused:    "CONNECTION_REFUSED",
		RepTTLExpired:           "TTL_EXPIRED",
		RepCommandNotSupported:  "COMMAND_NOT_SUPPORTED",
		RepAddrTypeNotSupported: "ADDR_TYPE_NOT_SUPPORTED",
	}
	for code, want := range cases {
		if got := RepString(code); got != want {
			t.Fatalf("RepString(0x%02X) = %q，期望 %q", code, got, want)
		}
	}
	if got := RepString(0x7F); got == "" || got == "SUCCESS" {
		t.Fatalf("未知码输出异常: %q", got)
	}
}

func TestUnspecificBind(t *testing.T) {
	b := UnspecificBind()
	raw, err := b.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(raw, []byte{ATYPIPv4, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("UnspecificBind wire = % x", raw)
	}
}
