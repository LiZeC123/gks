package protocol

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func testPSK() []byte { return bytes.Repeat([]byte{0x42}, PSKSize) }

func fixedClock(ts int64) func() time.Time {
	return func() time.Time { return time.Unix(ts, 0) }
}

func makeAuthRequest(psk []byte, tokenID uint16, ts uint64, nonce byte) AuthRequest {
	req := AuthRequest{TokenID: tokenID, Timestamp: ts}
	for i := range req.NonceC {
		req.NonceC[i] = nonce
	}
	copy(req.Proof[:], ComputeProof(psk, tokenID, req.NonceC[:], ts))
	return req
}

func newTestAuthenticator(t *testing.T, psk []byte, aead string, now int64, window time.Duration, cache *ReplayCache) *Authenticator {
	t.Helper()
	if cache == nil {
		var err error
		cache, err = NewReplayCache(64, 2*window)
		if err != nil {
			t.Fatalf("NewReplayCache: %v", err)
		}
	}
	a, err := NewAuthenticator(AuthenticatorConfig{
		HandshakeConfig: HandshakeConfig{
			PSK:  psk,
			AEAD: aead,
			Now:  fixedClock(now),
			Rand: bytes.NewReader(bytes.Repeat([]byte{0xB2}, AuthNonceSize*4)),
		},
		Window: window,
		Replay: cache,
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return a
}

func TestComputeProof(t *testing.T) {
	psk := testPSK()
	a := ComputeProof(psk, 1, bytes.Repeat([]byte{1}, AuthNonceSize), 1000)
	if len(a) != AuthProofSize {
		t.Fatalf("proof 长度 = %d", len(a))
	}
	b := ComputeProof(psk, 1, bytes.Repeat([]byte{1}, AuthNonceSize), 1000)
	if !bytes.Equal(a, b) {
		t.Fatal("相同输入应得到相同 proof")
	}
	c := ComputeProof(psk, 2, bytes.Repeat([]byte{1}, AuthNonceSize), 1000)
	if bytes.Equal(a, c) {
		t.Fatal("token_id 不同应得到不同 proof")
	}
	d := ComputeProof(psk, 1, bytes.Repeat([]byte{1}, AuthNonceSize), 1001)
	if bytes.Equal(a, d) {
		t.Fatal("时间戳不同应得到不同 proof")
	}
	e := ComputeProof(psk, 1, bytes.Repeat([]byte{2}, AuthNonceSize), 1000)
	if bytes.Equal(a, e) {
		t.Fatal("nonce 不同应得到不同 proof")
	}
	f := ComputeProof(bytes.Repeat([]byte{0x43}, PSKSize), 1, bytes.Repeat([]byte{1}, AuthNonceSize), 1000)
	if bytes.Equal(a, f) {
		t.Fatal("PSK 不同应得到不同 proof")
	}
}

func TestAuthRequestCodec(t *testing.T) {
	req := makeAuthRequest(testPSK(), 1, 1700000000, 0x77)
	raw := req.Marshal()
	if len(raw) != AuthRequestSize {
		t.Fatalf("编码长度 = %d，期望 %d", len(raw), AuthRequestSize)
	}
	got, err := ParseAuthRequest(raw)
	if err != nil {
		t.Fatalf("ParseAuthRequest: %v", err)
	}
	if got != req {
		t.Fatalf("round-trip 不一致: %+v vs %+v", got, req)
	}
	for _, n := range []int{0, AuthRequestSize - 1, AuthRequestSize + 1} {
		if _, err := ParseAuthRequest(make([]byte, n)); !errors.Is(err, ErrAuthMalformed) {
			t.Fatalf("长度 %d 的 err = %v", n, err)
		}
	}
}

func TestAuthResponseCodec(t *testing.T) {
	ok := AuthResponse{Code: AuthCodeOK}
	copy(ok.NonceS[:], bytes.Repeat([]byte{0x31}, AuthNonceSize))
	raw := ok.Marshal()
	if len(raw) != AuthResponseOKSize {
		t.Fatalf("成功响应长度 = %d，期望 %d", len(raw), AuthResponseOKSize)
	}
	got, err := ParseAuthResponse(raw)
	if err != nil {
		t.Fatalf("ParseAuthResponse: %v", err)
	}
	if got != ok {
		t.Fatalf("成功响应 round-trip 不一致: %+v", got)
	}

	for _, code := range []byte{AuthCodeBadToken, AuthCodeTimestamp, AuthCodeReplay, AuthCodeOther} {
		bad := AuthResponse{Code: code}
		raw := bad.Marshal()
		if len(raw) != 1 {
			t.Fatalf("失败响应长度 = %d", len(raw))
		}
		got, err := ParseAuthResponse(raw)
		if err != nil || got.Code != code {
			t.Fatalf("失败响应解析错误: %+v %v", got, err)
		}
	}

	// 长度与 code 必须匹配。
	if _, err := ParseAuthResponse([]byte{AuthCodeOK}); !errors.Is(err, ErrAuthMalformed) {
		t.Fatalf("成功码但只有 1 字节: %v", err)
	}
	badFailure := append([]byte{AuthCodeBadToken}, make([]byte, AuthNonceSize)...)
	if _, err := ParseAuthResponse(badFailure); !errors.Is(err, ErrAuthMalformed) {
		t.Fatalf("失败码却有 17 字节: %v", err)
	}
	if _, err := ParseAuthResponse(nil); !errors.Is(err, ErrAuthMalformed) {
		t.Fatalf("空响应: %v", err)
	}
}

func TestAuthenticatorHappyPathAllAEAD(t *testing.T) {
	for _, aead := range []string{AEADChaCha20Poly1305, AEADAES256GCM} {
		t.Run(aead, func(t *testing.T) {
			const now = int64(1700000000)
			psk := testPSK()
			init, err := NewInitiator(HandshakeConfig{
				PSK:  psk,
				AEAD: aead,
				Now:  fixedClock(now),
				Rand: bytes.NewReader(bytes.Repeat([]byte{0xA1}, AuthNonceSize*4)),
			})
			if err != nil {
				t.Fatalf("NewInitiator: %v", err)
			}
			req, err := init.NewRequest()
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			auth := newTestAuthenticator(t, psk, aead, now, 60*time.Second, nil)
			resp, err := auth.Verify(req)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if resp.Code != AuthCodeOK {
				t.Fatalf("code = %s", AuthCodeString(resp.Code))
			}

			clientKeys, err := init.Complete(req, resp)
			if err != nil {
				t.Fatalf("客户端派生密钥: %v", err)
			}
			serverKeys, err := auth.Complete(req, resp)
			if err != nil {
				t.Fatalf("服务端派生密钥: %v", err)
			}

			nonce := make([]byte, NonceSize)
			// C→S：客户端加密，服务端解密。
			ct := clientKeys.C2S.Seal(nil, nonce, []byte("来自客户端"), nil)
			pt, err := serverKeys.C2S.Open(nil, nonce, ct, nil)
			if err != nil {
				t.Fatalf("服务端解 C2S: %v", err)
			}
			if string(pt) != "来自客户端" {
				t.Fatalf("C2S 明文 = %q", pt)
			}
			// S→C：服务端加密，客户端解密。
			ct2 := serverKeys.S2C.Seal(nil, nonce, []byte("来自服务端"), nil)
			pt2, err := clientKeys.S2C.Open(nil, nonce, ct2, nil)
			if err != nil {
				t.Fatalf("客户端解 S2C: %v", err)
			}
			if string(pt2) != "来自服务端" {
				t.Fatalf("S2C 明文 = %q", pt2)
			}
			// 交叉方向必须失败。
			if _, err := clientKeys.C2S.Open(nil, nonce, ct2, nil); err == nil {
				t.Fatal("C2S 密钥解开了 S2C 密文")
			}
		})
	}
}

func TestAuthenticatorRejectsBadTokenID(t *testing.T) {
	const now = int64(1700000000)
	psk := testPSK()
	auth := newTestAuthenticator(t, psk, AEADChaCha20Poly1305, now, 60*time.Second, nil)
	resp, err := auth.Verify(makeAuthRequest(psk, 0x0002, uint64(now), 0x01))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Code != AuthCodeBadToken {
		t.Fatalf("code = %s", AuthCodeString(resp.Code))
	}
}

func TestAuthenticatorRejectsWrongPSKWithoutPollutingCache(t *testing.T) {
	const now = int64(1700000000)
	cache, err := NewReplayCache(16, 2*time.Minute)
	if err != nil {
		t.Fatalf("NewReplayCache: %v", err)
	}
	auth := newTestAuthenticator(t, testPSK(), AEADChaCha20Poly1305, now, 60*time.Second, cache)
	attacker := bytes.Repeat([]byte{0x43}, PSKSize)
	resp, err := auth.Verify(makeAuthRequest(attacker, DefaultTokenID, uint64(now), 0x02))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Code != AuthCodeBadToken {
		t.Fatalf("code = %s", AuthCodeString(resp.Code))
	}
	if n := cache.Len(); n != 0 {
		t.Fatalf("未通过 proof 的请求不应写入重放缓存，当前 %d 条", n)
	}
}

func TestAuthenticatorTimestampWindow(t *testing.T) {
	const now = int64(1700000000)
	psk := testPSK()
	cases := []struct {
		name   string
		offset int64
		want   byte
	}{
		{"刚好过去 60s", -60, AuthCodeOK},
		{"刚好未来 60s", +60, AuthCodeOK},
		{"过去 61s", -61, AuthCodeTimestamp},
		{"未来 61s", +61, AuthCodeTimestamp},
		{"过去 1 小时", -3600, AuthCodeTimestamp},
		{"未来 1 年", +365 * 24 * 3600, AuthCodeTimestamp},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := newTestAuthenticator(t, psk, AEADChaCha20Poly1305, now, 60*time.Second, nil)
			req := makeAuthRequest(psk, DefaultTokenID, uint64(now+tc.offset), byte(i+1))
			resp, err := auth.Verify(req)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if resp.Code != tc.want {
				t.Fatalf("code = %s，期望 %s", AuthCodeString(resp.Code), AuthCodeString(tc.want))
			}
		})
	}
}

func TestAuthenticatorRejectsReplay(t *testing.T) {
	const now = int64(1700000000)
	psk := testPSK()
	auth := newTestAuthenticator(t, psk, AEADChaCha20Poly1305, now, 60*time.Second, nil)
	req := makeAuthRequest(psk, DefaultTokenID, uint64(now), 0x09)

	first, err := auth.Verify(req)
	if err != nil {
		t.Fatalf("首次 Verify: %v", err)
	}
	if first.Code != AuthCodeOK {
		t.Fatalf("首次 code = %s", AuthCodeString(first.Code))
	}
	second, err := auth.Verify(req)
	if err != nil {
		t.Fatalf("二次 Verify: %v", err)
	}
	if second.Code != AuthCodeReplay {
		t.Fatalf("二次 code = %s，期望 REPLAY", AuthCodeString(second.Code))
	}
}

func TestReplayCacheEvictionAndTTL(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cache, err := NewReplayCache(2, time.Minute)
	if err != nil {
		t.Fatalf("NewReplayCache: %v", err)
	}
	nonce := func(b byte) [AuthNonceSize]byte {
		var n [AuthNonceSize]byte
		for i := range n {
			n[i] = b
		}
		return n
	}

	if cache.Seen(1, nonce(1), now) || cache.Seen(1, nonce(2), now) {
		t.Fatal("首次出现不应判定为重放")
	}
	if cache.Len() != 2 {
		t.Fatalf("Len = %d", cache.Len())
	}
	// 超过容量，最旧的被淘汰。
	cache.Seen(1, nonce(3), now)
	if cache.Len() != 2 {
		t.Fatalf("淘汰后 Len = %d", cache.Len())
	}
	if cache.Seen(1, nonce(1), now) {
		t.Fatal("最旧的条目应已被淘汰")
	}
	// TTL 过期后应被清理（此时缓存里是 nonce2/3/1 三条，容量 2）。
	later := now.Add(2 * time.Minute)
	if cache.Seen(1, nonce(9), later) {
		t.Fatal("新 nonce 首次出现不应判定为重放")
	}
	if cache.Len() != 1 {
		t.Fatalf("过期清理后 Len = %d，期望 1", cache.Len())
	}
}

func TestReplayCacheDistinguishesToken(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cache, err := NewReplayCache(8, time.Minute)
	if err != nil {
		t.Fatalf("NewReplayCache: %v", err)
	}
	var n [AuthNonceSize]byte
	if cache.Seen(1, n, now) {
		t.Fatal("首次不应为重放")
	}
	if !cache.Seen(1, n, now) {
		t.Fatal("相同 token+nonce 应判定为重放")
	}
	if cache.Seen(2, n, now) {
		t.Fatal("不同 token 的相同 nonce 不应被判重放")
	}
}

func TestReplayCacheInvalidConfigAndNil(t *testing.T) {
	if _, err := NewReplayCache(0, time.Minute); !errors.Is(err, ErrReplayCacheCfg) {
		t.Fatalf("capacity=0: %v", err)
	}
	if _, err := NewReplayCache(8, 0); !errors.Is(err, ErrReplayCacheCfg) {
		t.Fatalf("ttl=0: %v", err)
	}
	var nilCache *ReplayCache
	var n [AuthNonceSize]byte
	if nilCache.Seen(1, n, time.Now()) {
		t.Fatal("nil 缓存应始终返回 false")
	}
	if nilCache.Len() != 0 {
		t.Fatal("nil 缓存 Len 应为 0")
	}
}

func TestHandshakeConfigValidation(t *testing.T) {
	if _, err := NewInitiator(HandshakeConfig{PSK: testPSK()[:16]}); !errors.Is(err, ErrBadPSK) {
		t.Fatalf("短 psk: %v", err)
	}
	if _, err := NewInitiator(HandshakeConfig{PSK: testPSK(), AEAD: "rot13"}); !errors.Is(err, ErrUnknownAEAD) {
		t.Fatalf("未知算法: %v", err)
	}
	if _, err := NewAuthenticator(AuthenticatorConfig{HandshakeConfig: HandshakeConfig{PSK: testPSK()}}); !errors.Is(err, ErrReplayCacheCfg) {
		t.Fatalf("window=0: %v", err)
	}

	// 默认值：AEAD 为空 → chacha20；TokenID 为 0 → DefaultTokenID。
	init, err := NewInitiator(HandshakeConfig{PSK: testPSK()})
	if err != nil {
		t.Fatalf("NewInitiator: %v", err)
	}
	if init.AEAD() != AEADChaCha20Poly1305 {
		t.Fatalf("默认 AEAD = %s", init.AEAD())
	}
	req, err := init.NewRequest()
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if req.TokenID != DefaultTokenID {
		t.Fatalf("默认 token id = %d", req.TokenID)
	}
}

func TestCompleteRejectsNonOK(t *testing.T) {
	psk := testPSK()
	init, err := NewInitiator(HandshakeConfig{PSK: psk})
	if err != nil {
		t.Fatalf("NewInitiator: %v", err)
	}
	req, err := init.NewRequest()
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if _, err := init.Complete(req, AuthResponse{Code: AuthCodeReplay}); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("客户端: %v", err)
	}
	auth := newTestAuthenticator(t, psk, AEADChaCha20Poly1305, time.Now().Unix(), 60*time.Second, nil)
	if _, err := auth.Complete(req, AuthResponse{Code: AuthCodeBadToken}); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("服务端: %v", err)
	}
}

func TestAuthCodeString(t *testing.T) {
	if AuthCodeString(AuthCodeOK) != "OK" || AuthCodeString(AuthCodeReplay) != "REPLAY" {
		t.Fatal("AuthCodeString 输出不符")
	}
	if got := AuthCodeString(0x77); got == "" {
		t.Fatal("未知码应有可读输出")
	}
}
