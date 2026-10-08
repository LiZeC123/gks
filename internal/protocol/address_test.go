package protocol

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestAddressMarshalIPv4(t *testing.T) {
	a := Address{ATYP: ATYPIPv4, Host: "1.2.3.4", Port: 80}
	got, err := a.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := []byte{0x01, 1, 2, 3, 4, 0x00, 0x50}
	if !bytes.Equal(got, want) {
		t.Fatalf("wire = % x，期望 % x", got, want)
	}
}

func TestAddressMarshalDomain(t *testing.T) {
	a := Address{ATYP: ATYPDomain, Host: "example.com", Port: 443}
	got, err := a.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got[0] != ATYPDomain || got[1] != byte(len("example.com")) {
		t.Fatalf("前缀 = % x", got[:2])
	}
	if string(got[2:2+len("example.com")]) != "example.com" {
		t.Fatalf("域名编码错误: %q", got[2:2+len("example.com")])
	}
	if got[len(got)-2] != 0x01 || got[len(got)-1] != 0xBB {
		t.Fatalf("端口编码错误: % x", got[len(got)-2:])
	}
}

func TestAddressMarshalIPv6(t *testing.T) {
	a := Address{ATYP: ATYPIPv6, Host: "::1", Port: 53}
	got, err := a.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := append([]byte{ATYPIPv6}, net.ParseIP("::1").To16()...)
	want = append(want, 0x00, 0x35)
	if !bytes.Equal(got, want) {
		t.Fatalf("wire = % x，期望 % x", got, want)
	}
}

func TestAddressRoundTrip(t *testing.T) {
	cases := []Address{
		{ATYP: ATYPIPv4, Host: "127.0.0.1", Port: 2080},
		{ATYP: ATYPIPv4, Host: "0.0.0.0", Port: 0},
		{ATYP: ATYPDomain, Host: "www.baidu.com", Port: 80},
		{ATYP: ATYPDomain, Host: "a", Port: 1},
		{ATYP: ATYPIPv6, Host: "2001:db8::1", Port: 65535},
		{ATYP: ATYPIPv6, Host: "::", Port: 0},
	}
	for _, want := range cases {
		raw, err := want.Marshal()
		if err != nil {
			t.Fatalf("%v Marshal: %v", want, err)
		}
		got, n, err := ParseAddress(raw)
		if err != nil {
			t.Fatalf("%v ParseAddress: %v", want, err)
		}
		if n != len(raw) {
			t.Fatalf("%v 消耗字节 = %d，期望 %d", want, n, len(raw))
		}
		if got != want {
			t.Fatalf("round-trip = %+v，期望 %+v", got, want)
		}
	}
}

func TestAddressString(t *testing.T) {
	cases := []struct {
		addr Address
		want string
	}{
		{Address{ATYPIPv4, "1.2.3.4", 80}, "1.2.3.4:80"},
		{Address{ATYPDomain, "www.baidu.com", 443}, "www.baidu.com:443"},
		{Address{ATYPIPv6, "::1", 2080}, "[::1]:2080"},
	}
	for _, tc := range cases {
		if got := tc.addr.Address(); got != tc.want {
			t.Fatalf("Address() = %q，期望 %q", got, tc.want)
		}
		if got := tc.addr.String(); got != tc.want {
			t.Fatalf("String() = %q，期望 %q", got, tc.want)
		}
	}
}

func TestAddressFromHostPort(t *testing.T) {
	cases := []struct {
		host string
		port uint16
		want Address
	}{
		{"127.0.0.1", 80, Address{ATYPIPv4, "127.0.0.1", 80}},
		{"::1", 80, Address{ATYPIPv6, "::1", 80}},
		{"example.com", 80, Address{ATYPDomain, "example.com", 80}},
		{"localhost", 80, Address{ATYPDomain, "localhost", 80}},
		{"www.baidu.com", 443, Address{ATYPDomain, "www.baidu.com", 443}},
	}
	for _, tc := range cases {
		got, err := AddressFromHostPort(tc.host, tc.port)
		if err != nil {
			t.Fatalf("AddressFromHostPort(%q): %v", tc.host, err)
		}
		if got != tc.want {
			t.Fatalf("AddressFromHostPort(%q) = %+v，期望 %+v", tc.host, got, tc.want)
		}
	}
	if _, err := AddressFromHostPort("", 80); !errors.Is(err, ErrEmptyHost) {
		t.Fatalf("空主机: %v", err)
	}
	if _, err := AddressFromHostPort(strings.Repeat("a", MaxDomainLen+1), 80); !errors.Is(err, ErrHostTooLong) {
		t.Fatalf("超长域名: %v", err)
	}
}

func TestAddressParseErrors(t *testing.T) {
	longDomain := append([]byte{ATYPDomain, byte(MaxDomainLen)}, bytes.Repeat([]byte{'a'}, MaxDomainLen)...)
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"空输入", nil, ErrBadAddress},
		{"未知 ATYP", []byte{0x09, 1, 2}, ErrBadATYP},
		{"IPv4 截断", []byte{ATYPIPv4, 1, 2, 3}, ErrBadAddress},
		{"IPv4 缺端口", []byte{ATYPIPv4, 1, 2, 3, 4, 0x00}, ErrBadAddress},
		{"IPv6 截断", append([]byte{ATYPIPv6}, make([]byte, 10)...), ErrBadAddress},
		{"域名缺长度", []byte{ATYPDomain}, ErrBadAddress},
		{"域名长度为 0", []byte{ATYPDomain, 0x00, 0x00, 0x50}, ErrEmptyHost},
		{"域名截断", []byte{ATYPDomain, 5, 'a', 'b'}, ErrBadAddress},
		{"域名缺端口", append(longDomain, 0x00), ErrBadAddress},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ParseAddress(tc.in); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v，期望 %v", err, tc.want)
			}
		})
	}
}

func TestAddressMarshalErrors(t *testing.T) {
	cases := []struct {
		name string
		addr Address
		want error
	}{
		{"未知 ATYP", Address{ATYP: 0x09, Host: "1.2.3.4"}, ErrBadATYP},
		{"IPv4 类型但主机是域名", Address{ATYP: ATYPIPv4, Host: "example.com"}, ErrBadAddress},
		{"IPv6 类型但主机是 IPv4", Address{ATYP: ATYPIPv6, Host: "1.2.3.4"}, ErrBadAddress},
		{"域名类型但主机为空", Address{ATYP: ATYPDomain, Host: ""}, ErrEmptyHost},
		{"域名超长", Address{ATYP: ATYPDomain, Host: strings.Repeat("a", MaxDomainLen+1)}, ErrHostTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.addr.Marshal(); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v，期望 %v", err, tc.want)
			}
		})
	}
}

func TestParseAddressPartialConsumption(t *testing.T) {
	// ParseAddress 只吃自己需要的字节，剩余部分留给调用方（SOCKS5 报文可能带别的字段）。
	raw := []byte{ATYPIPv4, 1, 2, 3, 4, 0x00, 0x50, 0xAA, 0xBB}
	addr, n, err := ParseAddress(raw)
	if err != nil {
		t.Fatalf("ParseAddress: %v", err)
	}
	if n != 7 {
		t.Fatalf("消耗字节 = %d，期望 7", n)
	}
	if addr.Port != 80 {
		t.Fatalf("端口 = %d", addr.Port)
	}
}
