package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestDeriveKeyDeterministic(t *testing.T) {
	psk := bytes.Repeat([]byte{0x01}, PSKSize)
	salt := []byte("salty")
	a, err := DeriveKey(psk, salt, "info-a", 32)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	b, err := DeriveKey(psk, salt, "info-a", 32)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("相同输入应得到相同密钥")
	}
	if len(a) != 32 {
		t.Fatalf("长度 = %d", len(a))
	}
}

func TestDeriveKeyDomainSeparation(t *testing.T) {
	psk := bytes.Repeat([]byte{0x02}, PSKSize)
	salt := []byte("salt")
	c2s, err := DeriveKey(psk, salt, "gks-c2s-v1", 32)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	s2c, err := DeriveKey(psk, salt, "gks-s2c-v1", 32)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if bytes.Equal(c2s, s2c) {
		t.Fatal("不同 info 必须得到不同密钥")
	}
	// 会话密钥与传输层（kcp）密钥必须互不相关。
	kcpKey, err := DeriveKey(psk, salt, "gks-kcp-aes-128-gcm-v1", 16)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if bytes.Equal(kcpKey, c2s[:16]) {
		t.Fatal("不同用途的密钥不应相同")
	}
	// 不同 salt 必须得到不同密钥。
	otherSalt, err := DeriveKey(psk, []byte("other-salt"), "gks-c2s-v1", 32)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if bytes.Equal(otherSalt, c2s) {
		t.Fatal("不同 salt 应得到不同密钥")
	}
	short, err := DeriveKey(psk, salt, "gks-c2s-v1", 16)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if len(short) != 16 {
		t.Fatalf("长度 = %d", len(short))
	}
}

func TestDeriveKeyErrors(t *testing.T) {
	if _, err := DeriveKey(nil, nil, "x", 32); !errors.Is(err, ErrBadKeySize) {
		t.Fatalf("空 psk: %v", err)
	}
	if _, err := DeriveKey([]byte{1}, nil, "x", 0); !errors.Is(err, ErrBadKeySize) {
		t.Fatalf("长度 0: %v", err)
	}
}

func TestNewAEADVariants(t *testing.T) {
	key := bytes.Repeat([]byte{0x03}, AEADKeySize)
	for _, name := range []string{AEADChaCha20Poly1305, AEADAES256GCM} {
		c, err := NewAEAD(name, key)
		if err != nil {
			t.Fatalf("NewAEAD(%s): %v", name, err)
		}
		if c.NonceSize() != NonceSize {
			t.Fatalf("%s nonce = %d，期望 %d", name, c.NonceSize(), NonceSize)
		}
		if c.Overhead() != TagSize {
			t.Fatalf("%s overhead = %d，期望 %d", name, c.Overhead(), TagSize)
		}
		nonce := make([]byte, NonceSize)
		sealed := c.Seal(nil, nonce, []byte("hi"), []byte("aad"))
		if _, err := c.Open(nil, nonce, sealed, []byte("aad")); err != nil {
			t.Fatalf("%s open: %v", name, err)
		}
		if _, err := c.Open(nil, nonce, sealed, []byte("bad")); err == nil {
			t.Fatalf("%s 应因 AAD 不符而失败", name)
		}
	}
	if _, err := NewAEAD(AEADChaCha20Poly1305, key[:16]); !errors.Is(err, ErrBadKeySize) {
		t.Fatalf("错误密钥长度: %v", err)
	}
	if _, err := NewAEAD("rot13", key); !errors.Is(err, ErrUnknownAEAD) {
		t.Fatalf("未知算法: %v", err)
	}
	if ValidAEAD("rot13") {
		t.Fatal("ValidAEAD 判定错误")
	}
}

func TestNonceCounterSequence(t *testing.T) {
	n := NewNonceCounter()
	for want := uint64(0); want < 5; want++ {
		nonce, err := n.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if len(nonce) != NonceSize {
			t.Fatalf("nonce 长度 = %d", len(nonce))
		}
		if !bytes.Equal(nonce[:noncePrefixSize], make([]byte, noncePrefixSize)) {
			t.Fatalf("nonce 前缀应为 0: %x", nonce[:noncePrefixSize])
		}
		if got := binary.BigEndian.Uint64(nonce[noncePrefixSize:]); got != want {
			t.Fatalf("计数器 = %d，期望 %d", got, want)
		}
	}
	if n.Count() != 5 {
		t.Fatalf("Count = %d", n.Count())
	}
}

func TestNonceCounterDoesNotAlias(t *testing.T) {
	n := NewNonceCounter()
	a, err := n.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	b, err := n.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if &a[0] == &b[0] {
		t.Fatal("两次 Next 不应共享底层数组")
	}
}

func TestNonceCounterExhausted(t *testing.T) {
	n := NewNonceCounter()
	n.next = math.MaxUint64
	if _, err := n.Next(); !errors.Is(err, ErrNonceExhausted) {
		t.Fatalf("err = %v，期望 ErrNonceExhausted", err)
	}

	// 最后一个可用值仍须可用，随后才耗尽。
	m := NewNonceCounter()
	m.next = math.MaxUint64 - 1
	nonce, err := m.Next()
	if err != nil {
		t.Fatalf("最后一个可用 nonce: %v", err)
	}
	if got := binary.BigEndian.Uint64(nonce[noncePrefixSize:]); got != math.MaxUint64-1 {
		t.Fatalf("计数器 = %d", got)
	}
	if _, err := m.Next(); !errors.Is(err, ErrNonceExhausted) {
		t.Fatalf("耗尽后 err = %v", err)
	}
}

func TestSessionKeysDirectionSeparation(t *testing.T) {
	psk := bytes.Repeat([]byte{0x04}, PSKSize)
	nonceC := bytes.Repeat([]byte{0x05}, AuthNonceSize)
	nonceS := bytes.Repeat([]byte{0x06}, AuthNonceSize)
	keys, err := NewSessionKeys(AEADChaCha20Poly1305, psk, nonceC, nonceS)
	if err != nil {
		t.Fatalf("NewSessionKeys: %v", err)
	}
	// 同一 nonce、同一明文，两个方向必须产生不同密文（因为密钥不同）。
	nonce := make([]byte, NonceSize)
	plain := []byte("same-plaintext")
	ct1 := keys.C2S.Seal(nil, nonce, plain, nil)
	ct2 := keys.S2C.Seal(nil, nonce, plain, nil)
	if bytes.Equal(ct1, ct2) {
		t.Fatal("双向密钥相同，nonce 冲突")
	}
	// 交叉解密必须失败。
	if _, err := keys.S2C.Open(nil, nonce, ct1, nil); err == nil {
		t.Fatal("用 S2C 密钥解开了 C2S 的密文")
	}

	// 相同 nonce_c/nonce_s 下的派生必须可复现。
	again, err := NewSessionKeys(AEADChaCha20Poly1305, psk, nonceC, nonceS)
	if err != nil {
		t.Fatalf("NewSessionKeys: %v", err)
	}
	if !bytes.Equal(again.C2S.Seal(nil, nonce, plain, nil), ct1) {
		t.Fatal("派生不可复现")
	}
}

func TestSessionKeysRejectsBadInputs(t *testing.T) {
	psk := bytes.Repeat([]byte{0x07}, PSKSize)
	nonceC := bytes.Repeat([]byte{0x08}, AuthNonceSize)
	nonceS := bytes.Repeat([]byte{0x09}, AuthNonceSize)
	if _, err := NewSessionKeys(AEADChaCha20Poly1305, psk[:16], nonceC, nonceS); !errors.Is(err, ErrBadKeySize) {
		t.Fatalf("短 psk: %v", err)
	}
	if _, err := NewSessionKeys(AEADChaCha20Poly1305, psk, nonceC[:8], nonceS); !errors.Is(err, ErrBadAuthNonce) {
		t.Fatalf("短 nonce: %v", err)
	}
	if _, err := NewSessionKeys("rot13", psk, nonceC, nonceS); !errors.Is(err, ErrUnknownAEAD) {
		t.Fatalf("未知算法: %v", err)
	}
}

func TestRandomBytes(t *testing.T) {
	a, err := RandomBytes(16)
	if err != nil {
		t.Fatalf("RandomBytes: %v", err)
	}
	b, err := RandomBytes(16)
	if err != nil {
		t.Fatalf("RandomBytes: %v", err)
	}
	if len(a) != 16 || bytes.Equal(a, b) {
		t.Fatal("随机数长度或随机性异常")
	}
}
