package client

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/LiZeC123/gks/internal/protocol"
)

func TestNegotiateNoAuth(t *testing.T) {
	rw := &rwBuffer{in: []byte{0x05, 0x01, 0x00}}
	if err := Negotiate(rw); err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if !bytes.Equal(rw.out, []byte{0x05, 0x00}) {
		t.Fatalf("回复 = % x，期望 05 00", rw.out)
	}
}

func TestNegotiatePicksNoAuthAmongSeveral(t *testing.T) {
	rw := &rwBuffer{in: []byte{0x05, 0x03, 0x02, 0x01, 0x00}}
	if err := Negotiate(rw); err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if !bytes.Equal(rw.out, []byte{0x05, 0x00}) {
		t.Fatalf("回复 = % x", rw.out)
	}
}

func TestNegotiateNoAcceptableMethod(t *testing.T) {
	rw := &rwBuffer{in: []byte{0x05, 0x02, 0x02, 0x80}}
	err := Negotiate(rw)
	if !errors.Is(err, ErrNoAcceptableMethod) {
		t.Fatalf("err = %v，期望 ErrNoAcceptableMethod", err)
	}
	if !bytes.Equal(rw.out, []byte{0x05, 0xFF}) {
		t.Fatalf("回复 = % x，期望 05 FF", rw.out)
	}
}

func TestNegotiateZeroMethods(t *testing.T) {
	rw := &rwBuffer{in: []byte{0x05, 0x00}}
	if err := Negotiate(rw); !errors.Is(err, ErrNoAcceptableMethod) {
		t.Fatalf("err = %v", err)
	}
	if len(rw.out) != 0 {
		t.Fatalf("不应回复任何字节: % x", rw.out)
	}
}

func TestNegotiateBadVersion(t *testing.T) {
	rw := &rwBuffer{in: []byte{0x04, 0x01, 0x00}}
	if err := Negotiate(rw); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("err = %v，期望 ErrBadVersion", err)
	}
}

func TestNegotiateShortRead(t *testing.T) {
	rw := &rwBuffer{in: []byte{0x05}}
	if err := Negotiate(rw); err == nil {
		t.Fatal("截断报文应当报错")
	}
	// 声明了 3 个方法却只给 1 个。
	rw = &rwBuffer{in: []byte{0x05, 0x03, 0x00}}
	if err := Negotiate(rw); err == nil {
		t.Fatal("方法列表截断应当报错")
	}
}

func TestReadRequestIPv4(t *testing.T) {
	in := []byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0x08, 0x1A}
	target, err := ReadRequest(bytes.NewReader(in))
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	want := Target{ATYP: protocol.ATYPIPv4, Host: "127.0.0.1", Port: 2074}
	if target != want {
		t.Fatalf("target = %+v，期望 %+v", target, want)
	}
}

func TestReadRequestDomain(t *testing.T) {
	host := "www.baidu.com"
	in := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, host...)
	in = append(in, 0x00, 0x50)
	target, err := ReadRequest(bytes.NewReader(in))
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	want := Target{ATYP: protocol.ATYPDomain, Host: host, Port: 80}
	if target != want {
		t.Fatalf("target = %+v，期望 %+v", target, want)
	}
}

func TestReadRequestIPv6(t *testing.T) {
	in := append([]byte{0x05, 0x01, 0x00, 0x04}, make([]byte, 16)...)
	in[3+1+15] = 0x01 // ::1
	in = append(in, 0x1F, 0x90)
	target, err := ReadRequest(bytes.NewReader(in))
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if target.ATYP != protocol.ATYPIPv6 || target.Host != "::1" || target.Port != 8080 {
		t.Fatalf("target = %+v", target)
	}
}

func TestReadRequestRejectsNonConnect(t *testing.T) {
	for _, cmd := range []byte{cmdBind, cmdUDPAssoc} {
		in := []byte{0x05, cmd, 0x00, 0x01, 127, 0, 0, 1, 0x00, 0x50}
		if _, err := ReadRequest(bytes.NewReader(in)); !errors.Is(err, ErrCmdNotSupported) {
			t.Fatalf("CMD=0x%02X err = %v，期望 ErrCmdNotSupported", cmd, err)
		}
	}
}

func TestReadRequestRejectsBadReserved(t *testing.T) {
	in := []byte{0x05, 0x01, 0x01, 0x01, 127, 0, 0, 1, 0x00, 0x50}
	if _, err := ReadRequest(bytes.NewReader(in)); !errors.Is(err, ErrBadReserved) {
		t.Fatalf("err = %v，期望 ErrBadReserved", err)
	}
}

func TestReadRequestRejectsBadVersionAndATYP(t *testing.T) {
	in := []byte{0x04, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0x00, 0x50}
	if _, err := ReadRequest(bytes.NewReader(in)); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("err = %v，期望 ErrBadVersion", err)
	}
	in = []byte{0x05, 0x01, 0x00, 0x09, 127, 0, 0, 1, 0x00, 0x50}
	if _, err := ReadRequest(bytes.NewReader(in)); !errors.Is(err, protocol.ErrBadATYP) {
		t.Fatalf("err = %v，期望 ErrBadATYP", err)
	}
}

func TestReadRequestShortInput(t *testing.T) {
	cases := [][]byte{
		{0x05, 0x01},
		{0x05, 0x01, 0x00}, // 缺 ATYP
		{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0x00},     // IPv4 缺端口 1 字节
		{0x05, 0x01, 0x00, 0x03, 0x05, 'a', 'b'},         // 域名截断
		{0x05, 0x01, 0x00, 0x03, 0x00, 0x00, 0x50},       // 域名长度为 0
		{0x05, 0x01, 0x00, 0x04, 0, 0, 0, 0, 0, 0, 0, 0}, // IPv6 截断
	}
	for i, in := range cases {
		if _, err := ReadRequest(bytes.NewReader(in)); err == nil {
			t.Fatalf("第 %d 组截断报文应当报错", i+1)
		}
	}
}

func TestWriteReplyLayout(t *testing.T) {
	var buf bytes.Buffer
	bind := Target{ATYP: protocol.ATYPIPv4, Host: "10.0.0.1", Port: 40000}
	if err := WriteReply(&buf, protocol.RepSuccess, bind); err != nil {
		t.Fatalf("WriteReply: %v", err)
	}
	want := []byte{0x05, 0x00, 0x00, 0x01, 10, 0, 0, 1, 0x9C, 0x40}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("回复 = % x，期望 % x", buf.Bytes(), want)
	}
}

func TestWriteReplyDomainBind(t *testing.T) {
	var buf bytes.Buffer
	bind := Target{ATYP: protocol.ATYPDomain, Host: "example.com", Port: 443}
	if err := WriteReply(&buf, protocol.RepGeneralFailure, bind); err != nil {
		t.Fatalf("WriteReply: %v", err)
	}
	got := buf.Bytes()
	if got[0] != 0x05 || got[1] != protocol.RepGeneralFailure || got[2] != 0x00 {
		t.Fatalf("前三字节 = % x", got[:3])
	}
	if got[3] != protocol.ATYPDomain {
		t.Fatalf("ATYP = 0x%02X", got[3])
	}
	if !strings.Contains(string(got), "example.com") {
		t.Fatalf("BND 域名缺失: % x", got)
	}
}

func TestWriteReplyFallsBackOnBadBind(t *testing.T) {
	var buf bytes.Buffer
	// 非法 BND（未知 ATYP）时退回占位地址，保证失败码仍然送达。
	if err := WriteReply(&buf, protocol.RepHostUnreachable, Target{ATYP: 0x09}); err != nil {
		t.Fatalf("WriteReply: %v", err)
	}
	want := []byte{0x05, protocol.RepHostUnreachable, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("回复 = % x，期望 % x", buf.Bytes(), want)
	}
}

func TestReplyForError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode byte
		wantOK   bool
	}{
		{"无错误", nil, protocol.RepSuccess, true},
		{"非 CONNECT 命令", ErrCmdNotSupported, protocol.RepCommandNotSupported, true},
		{"地址类型不支持", protocol.ErrBadATYP, protocol.RepAddrTypeNotSupported, true},
		{"报文截断", ErrShortRequest, protocol.RepGeneralFailure, false},
		{"版本不对", ErrBadVersion, protocol.RepGeneralFailure, false},
		{"方法协商失败", ErrNoAcceptableMethod, protocol.RepGeneralFailure, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, ok := ReplyForError(tc.err)
			if code != tc.wantCode || ok != tc.wantOK {
				t.Fatalf("ReplyForError = (0x%02X, %v)，期望 (0x%02X, %v)", code, ok, tc.wantCode, tc.wantOK)
			}
		})
	}
}

// rwBuffer 是一个只进不出的 io.ReadWriter，用于协商类测试。
type rwBuffer struct {
	in  []byte
	out []byte
}

func (b *rwBuffer) Read(p []byte) (int, error) {
	if len(b.in) == 0 {
		return 0, errors.New("EOF")
	}
	n := copy(p, b.in)
	b.in = b.in[n:]
	return n, nil
}

func (b *rwBuffer) Write(p []byte) (int, error) {
	b.out = append(b.out, p...)
	return len(p), nil
}
