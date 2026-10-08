package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func mustPlain(t *testing.T, f Frame) []byte {
	t.Helper()
	raw, err := MarshalPlain(f)
	if err != nil {
		t.Fatalf("MarshalPlain: %v", err)
	}
	return raw
}

func TestMarshalPlainRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		f    Frame
	}{
		{"空 payload 控制帧", Frame{Type: TypePing, StreamID: 0}},
		{"1 字节 payload", Frame{Type: TypeData, StreamID: 1, Payload: []byte{0xAB}}},
		{"普通数据帧", Frame{Type: TypeData, StreamID: 0x01020304, Payload: []byte("hello gks")}},
		{"16KB payload", Frame{Type: TypeData, StreamID: 7, Payload: bytes.Repeat([]byte{0x5A}, MaxDataPayload)}},
		{"含 0x00 与高位字节", Frame{Type: TypeConnectReq, StreamID: 3, Payload: []byte{0x00, 0xFF, 0x00, 0x7F}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := mustPlain(t, tc.f)
			if len(raw) != HeaderSize+BodyPrefixSize+len(tc.f.Payload) {
				t.Fatalf("帧长 = %d，期望 %d", len(raw), HeaderSize+BodyPrefixSize+len(tc.f.Payload))
			}
			if raw[0] != Magic0 || raw[1] != Magic1 {
				t.Fatalf("magic = %x %x", raw[0], raw[1])
			}
			if raw[2] != Version {
				t.Fatalf("ver = %x", raw[2])
			}
			if raw[3] != 0 {
				t.Fatalf("flags = %x", raw[3])
			}
			if got := binary.BigEndian.Uint32(raw[4:HeaderSize]); int(got) != BodyPrefixSize+len(tc.f.Payload) {
				t.Fatalf("length 字段 = %d", got)
			}
			got, err := UnmarshalPlain(raw)
			if err != nil {
				t.Fatalf("UnmarshalPlain: %v", err)
			}
			if got.Type != tc.f.Type || got.StreamID != tc.f.StreamID || !bytes.Equal(got.Payload, tc.f.Payload) {
				t.Fatalf("round-trip 不一致: %+v vs %+v", got, tc.f)
			}
		})
	}
}

func TestMarshalPlainTooLarge(t *testing.T) {
	f := Frame{Type: TypeData, StreamID: 1, Payload: bytes.Repeat([]byte{1}, MaxFrameBody)}
	if _, err := MarshalPlain(f); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v，期望 ErrFrameTooLarge", err)
	}
}

func TestParseHeaderErrors(t *testing.T) {
	valid := Header{Length: 5}.Marshal()
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"长度不足", valid[:5], ErrShortFrame},
		{"magic 错误", []byte{'X', 'C', Version, 0, 0, 0, 0, 5}, ErrBadMagic},
		{"版本错误", []byte{Magic0, Magic1, 0x02, 0, 0, 0, 0, 5}, ErrBadVersion},
		{"flags 非 0", []byte{Magic0, Magic1, Version, 0x01, 0, 0, 0, 5}, ErrBadFlags},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseHeader(tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v，期望 %v", err, tc.want)
			}
		})
	}
}

func TestParseBodyErrors(t *testing.T) {
	if _, err := ParseBody([]byte{0x05, 0x00}); !errors.Is(err, ErrShortFrame) {
		t.Fatalf("短 body: %v", err)
	}
	if _, err := ParseBody([]byte{0x7E, 0, 0, 0, 1}); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("未知类型: %v", err)
	}
}

func TestReadFrame(t *testing.T) {
	f1 := Frame{Type: TypePing, StreamID: 0, Payload: []byte("p1")}
	f2 := Frame{Type: TypeData, StreamID: 9, Payload: bytes.Repeat([]byte{7}, 1000)}
	stream := append(mustPlain(t, f1), mustPlain(t, f2)...)
	r := bytes.NewReader(stream)

	h1, b1, err := ReadFrame(r, MaxFrameBody)
	if err != nil {
		t.Fatalf("读第 1 帧: %v", err)
	}
	if int(h1.Length) != len(b1) {
		t.Fatalf("长度字段 %d != body %d", h1.Length, len(b1))
	}
	got1, err := ParseBody(b1)
	if err != nil || got1.Type != TypePing || string(got1.Payload) != "p1" {
		t.Fatalf("第 1 帧解析错误: %+v %v", got1, err)
	}

	_, b2, err := ReadFrame(r, MaxFrameBody)
	if err != nil {
		t.Fatalf("读第 2 帧: %v", err)
	}
	got2, err := ParseBody(b2)
	if err != nil || got2.StreamID != 9 || len(got2.Payload) != 1000 {
		t.Fatalf("第 2 帧解析错误: %+v %v", got2, err)
	}

	if _, _, err := ReadFrame(r, MaxFrameBody); !errors.Is(err, io.EOF) {
		t.Fatalf("流末尾 err = %v，期望 EOF", err)
	}
}

func TestReadFramePartialBody(t *testing.T) {
	raw := mustPlain(t, Frame{Type: TypeData, StreamID: 1, Payload: bytes.Repeat([]byte{1}, 100)})
	r := bytes.NewReader(raw[:len(raw)-10])
	if _, _, err := ReadFrame(r, MaxFrameBody); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v，期望 ErrUnexpectedEOF", err)
	}
}

func TestReadFrameTooLargeDoesNotBlock(t *testing.T) {
	// 伪造一个超大 Length，但后面没有任何字节：必须立即报错而不是等待。
	hdr := Header{Length: 1 << 30}.Marshal()
	r := bytes.NewReader(hdr[:])
	_, _, err := ReadFrame(r, MaxFrameBody)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v，期望 ErrFrameTooLarge", err)
	}
}

func TestUnmarshalPlainLengthMismatch(t *testing.T) {
	raw := mustPlain(t, Frame{Type: TypeData, StreamID: 1, Payload: []byte("abcd")})
	if _, err := UnmarshalPlain(raw[:len(raw)-1]); !errors.Is(err, ErrBadLength) {
		t.Fatalf("err = %v，期望 ErrBadLength", err)
	}
}

// secureFixture 构造一对同步的读写计数器与密钥。
func secureFixture(t *testing.T) (Cipher, Cipher, *NonceCounter, *NonceCounter) {
	t.Helper()
	psk := bytes.Repeat([]byte{0x11}, PSKSize)
	nonceC := bytes.Repeat([]byte{0x22}, AuthNonceSize)
	nonceS := bytes.Repeat([]byte{0x33}, AuthNonceSize)
	keys, err := NewSessionKeys(AEADChaCha20Poly1305, psk, nonceC, nonceS)
	if err != nil {
		t.Fatalf("NewSessionKeys: %v", err)
	}
	return keys.C2S, keys.C2S, NewNonceCounter(), NewNonceCounter()
}

func TestSecureRoundTripAndNoPlaintextLeak(t *testing.T) {
	seal, open, writeNonce, readNonce := secureFixture(t)
	f := Frame{Type: TypeData, StreamID: 0xDEADBEEF, Payload: []byte("secret-payload-marker")}

	raw, err := MarshalSecure(seal, writeNonce, f)
	if err != nil {
		t.Fatalf("MarshalSecure: %v", err)
	}
	if bytes.Contains(raw, []byte("secret-payload-marker")) {
		t.Fatal("安全态帧里出现了明文 payload")
	}
	body := raw[HeaderSize:]
	if binary.BigEndian.Uint32(body[1:BodyPrefixSize]) == f.StreamID {
		t.Fatal("安全态帧里出现了明文 StreamID")
	}
	if body[0] == byte(f.Type) {
		t.Fatal("安全态帧体第一个字节恰好等于 Type")
	}

	hdr, err := ParseHeader(raw)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	got, err := UnmarshalSecure(open, readNonce, hdr, raw[HeaderSize:])
	if err != nil {
		t.Fatalf("UnmarshalSecure: %v", err)
	}
	if got.Type != f.Type || got.StreamID != f.StreamID || !bytes.Equal(got.Payload, f.Payload) {
		t.Fatalf("round-trip 不一致: %+v", got)
	}
}

func TestSecureDetectsTampering(t *testing.T) {
	seal, open, writeNonce, _ := secureFixture(t)
	f := Frame{Type: TypeData, StreamID: 5, Payload: []byte("payload")}
	raw, err := MarshalSecure(seal, writeNonce, f)
	if err != nil {
		t.Fatalf("MarshalSecure: %v", err)
	}

	t.Run("篡改密文体", func(t *testing.T) {
		bad := append([]byte(nil), raw...)
		bad[HeaderSize+3] ^= 0x01
		hdr, err := ParseHeader(bad)
		if err != nil {
			t.Fatalf("ParseHeader: %v", err)
		}
		if _, err := UnmarshalSecure(open, NewNonceCounter(), hdr, bad[HeaderSize:]); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("err = %v，期望 ErrDecrypt", err)
		}
	})

	t.Run("篡改 magic", func(t *testing.T) {
		bad := append([]byte(nil), raw...)
		bad[0] = 'X'
		if _, err := ParseHeader(bad); !errors.Is(err, ErrBadMagic) {
			t.Fatalf("err = %v，期望 ErrBadMagic", err)
		}
	})

	t.Run("篡改版本", func(t *testing.T) {
		bad := append([]byte(nil), raw...)
		bad[2] = 0x09
		if _, err := ParseHeader(bad); !errors.Is(err, ErrBadVersion) {
			t.Fatalf("err = %v，期望 ErrBadVersion", err)
		}
	})

	t.Run("篡改 flags", func(t *testing.T) {
		bad := append([]byte(nil), raw...)
		bad[3] = 0x80
		if _, err := ParseHeader(bad); !errors.Is(err, ErrBadFlags) {
			t.Fatalf("err = %v，期望 ErrBadFlags", err)
		}
	})

	t.Run("篡改 length 字段被 AAD 拦下", func(t *testing.T) {
		bad := append([]byte(nil), raw...)
		binary.BigEndian.PutUint32(bad[4:HeaderSize], uint32(len(bad)-HeaderSize+1))
		hdr, err := ParseHeader(bad)
		if err != nil {
			t.Fatalf("ParseHeader: %v", err)
		}
		// 要么长度不一致先报错，要么 AAD 校验失败，两者都不能接受。
		if _, err := UnmarshalSecure(open, NewNonceCounter(), hdr, bad[HeaderSize:]); err == nil {
			t.Fatal("篡改 length 后仍解密成功")
		}
	})

	t.Run("长度一致但 length 被改（AAD 生效）", func(t *testing.T) {
		// 保持 body 长度不变，只把 Length 字段改小 1：这样长度检查也会失败。
		bad := append([]byte(nil), raw...)
		binary.BigEndian.PutUint32(bad[4:HeaderSize], uint32(len(bad)-HeaderSize-1))
		hdr, err := ParseHeader(bad)
		if err != nil {
			t.Fatalf("ParseHeader: %v", err)
		}
		if _, err := UnmarshalSecure(open, NewNonceCounter(), hdr, bad[HeaderSize:]); err == nil {
			t.Fatal("篡改 length 后仍解密成功")
		}
	})
}

func TestSecureWrongKeyFails(t *testing.T) {
	seal, _, writeNonce, _ := secureFixture(t)
	otherKeys, err := NewSessionKeys(AEADChaCha20Poly1305, bytes.Repeat([]byte{0x99}, PSKSize),
		bytes.Repeat([]byte{0x22}, AuthNonceSize), bytes.Repeat([]byte{0x33}, AuthNonceSize))
	if err != nil {
		t.Fatalf("NewSessionKeys: %v", err)
	}
	raw, err := MarshalSecure(seal, writeNonce, Frame{Type: TypeData, StreamID: 1, Payload: []byte("x")})
	if err != nil {
		t.Fatalf("MarshalSecure: %v", err)
	}
	hdr, err := ParseHeader(raw)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if _, err := UnmarshalSecure(otherKeys.C2S, NewNonceCounter(), hdr, raw[HeaderSize:]); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("err = %v，期望 ErrDecrypt", err)
	}
}

func TestSecureNonceAdvancesOnFailure(t *testing.T) {
	// 解密失败也必须消耗一个 nonce，否则会与发送方错位。
	_, open, _, readNonce := secureFixture(t)
	before := readNonce.Count()
	raw, err := MarshalSecure(mustCipher(t), NewNonceCounter(), Frame{Type: TypeData, StreamID: 1, Payload: []byte("y")})
	if err != nil {
		t.Fatalf("MarshalSecure: %v", err)
	}
	hdr, err := ParseHeader(raw)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if _, err := UnmarshalSecure(open, readNonce, hdr, raw[HeaderSize:]); err == nil {
		t.Fatal("不同密钥却解密成功")
	}
	if readNonce.Count() != before+1 {
		t.Fatalf("计数器 = %d，期望 %d", readNonce.Count(), before+1)
	}
}

func mustCipher(t *testing.T) Cipher {
	t.Helper()
	keys, err := NewSessionKeys(AEADChaCha20Poly1305, bytes.Repeat([]byte{0x44}, PSKSize),
		bytes.Repeat([]byte{0x55}, AuthNonceSize), bytes.Repeat([]byte{0x66}, AuthNonceSize))
	if err != nil {
		t.Fatalf("NewSessionKeys: %v", err)
	}
	return keys.C2S
}

func TestTypeString(t *testing.T) {
	if got := TypeData.String(); got != "DATA" {
		t.Fatalf("TypeData.String() = %q", got)
	}
	if got := Type(0x7E).String(); !strings.HasPrefix(got, "UNKNOWN") {
		t.Fatalf("未知类型 String() = %q", got)
	}
	if Type(0x7E).Valid() || !TypeTestEcho.Valid() {
		t.Fatal("Valid() 判定不符合预期")
	}
}
