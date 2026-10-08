// Package protocol 定义 gks 的帧格式（proto v1）、AEAD 封装与认证握手。
//
// 帧结构（dev.md §3.2）：
//
//	+--------+------+-------+----------+-------------------------------+
//	| Magic  | Ver  | Flags | Length   | Body（Length 字节）            |
//	| 2B     | 1B   | 1B    | 4B (BE)  |                               |
//	+--------+------+-------+----------+-------------------------------+
//	  明文 8B，同时作为 AEAD 的 AAD
//
// Body 逻辑结构：Type(1B) | StreamID(4B, BE) | Payload(N)
//
// 两种编码态：
//   - 握手态（AUTH 完成前）：Body 明文。
//   - 安全态（AUTH 成功后）：Body 为 AEAD 密文，Type 与 StreamID 不再外露。
//
// 安全态下解密/校验失败一律视为致命错误（由调用方断开 Session），禁止降级为明文解析。
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// 协议常量。
const (
	Magic0 byte = 0x4B // 'K'
	Magic1 byte = 0x43 // 'C'

	Version byte = 0x01

	// HeaderSize 是明文头长度：Magic(2) + Ver(1) + Flags(1) + Length(4)。
	HeaderSize = 8
	// BodyPrefixSize 是帧体前缀长度：Type(1) + StreamID(4)。
	BodyPrefixSize = 5
	// TagSize 是 AEAD 认证标签长度（ChaCha20-Poly1305 与 AES-256-GCM 均为 16B）。
	TagSize = 16

	// MaxFrameBody 是单帧帧体长度上限，超过即断开（防伪造长度导致挂起）。
	MaxFrameBody = 65536
	// MaxDataPayload 是单个 DATA 帧的 Payload 上限，上层写入更大数据须拆帧。
	MaxDataPayload = 16384
)

// 帧解析错误。全部为哨兵错误，便于调用方用 errors.Is 判断。
var (
	ErrShortFrame    = errors.New("protocol: 帧长度不足")
	ErrBadMagic      = errors.New("protocol: magic 不匹配")
	ErrBadVersion    = errors.New("protocol: 版本不支持")
	ErrBadFlags      = errors.New("protocol: v1 要求 flags 为 0")
	ErrFrameTooLarge = errors.New("protocol: 帧体超过上限")
	ErrBadLength     = errors.New("protocol: 长度字段与实际字节数不一致")
	ErrUnknownType   = errors.New("protocol: 未知帧类型")
	ErrDecrypt       = errors.New("protocol: 解密或完整性校验失败")
)

// Type 是帧类型。
type Type uint8

// 帧类型定义（dev.md §3.3）。
const (
	TypeAuthReq     Type = 0x01
	TypeAuthResp    Type = 0x02
	TypeConnectReq  Type = 0x03
	TypeConnectResp Type = 0x04
	TypeData        Type = 0x05
	TypeFin         Type = 0x06
	TypeRst         Type = 0x07
	TypePing        Type = 0x08
	TypePong        Type = 0x09
	TypeGoAway      Type = 0x0A
	TypeError       Type = 0x0B

	// TypeTestEcho 仅供测试与阶段二的 Session 级 echo 使用，不属于线上协议的一部分。
	// 阶段三接入 SOCKS5 后不再使用。
	TypeTestEcho Type = 0xF0
)

// Valid 报告 t 是否属于 proto v1 已定义的类型。
func (t Type) Valid() bool {
	switch t {
	case TypeAuthReq, TypeAuthResp, TypeConnectReq, TypeConnectResp,
		TypeData, TypeFin, TypeRst, TypePing, TypePong, TypeGoAway, TypeError,
		TypeTestEcho:
		return true
	default:
		return false
	}
}

func (t Type) String() string {
	switch t {
	case TypeAuthReq:
		return "AUTH_REQ"
	case TypeAuthResp:
		return "AUTH_RESP"
	case TypeConnectReq:
		return "CONNECT_REQ"
	case TypeConnectResp:
		return "CONNECT_RESP"
	case TypeData:
		return "DATA"
	case TypeFin:
		return "FIN"
	case TypeRst:
		return "RST"
	case TypePing:
		return "PING"
	case TypePong:
		return "PONG"
	case TypeGoAway:
		return "GOAWAY"
	case TypeError:
		return "ERROR"
	case TypeTestEcho:
		return "TEST_ECHO"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02X)", uint8(t))
	}
}

// Header 是帧的明文头，同时作为 AEAD 的 AAD。
type Header struct {
	Flags  byte
	Length uint32
}

// Marshal 把帧头编码为 8 字节。
func (h Header) Marshal() [HeaderSize]byte {
	var b [HeaderSize]byte
	b[0], b[1] = Magic0, Magic1
	b[2] = Version
	b[3] = h.Flags
	binary.BigEndian.PutUint32(b[4:HeaderSize], h.Length)
	return b
}

// ParseHeader 解析并校验 8 字节明文头。
func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("%w: 需要 %d 字节，实际 %d", ErrShortFrame, HeaderSize, len(b))
	}
	if b[0] != Magic0 || b[1] != Magic1 {
		return Header{}, fmt.Errorf("%w: 0x%02X 0x%02X", ErrBadMagic, b[0], b[1])
	}
	if b[2] != Version {
		return Header{}, fmt.Errorf("%w: 0x%02X", ErrBadVersion, b[2])
	}
	if b[3] != 0 {
		return Header{}, fmt.Errorf("%w: 0x%02X", ErrBadFlags, b[3])
	}
	return Header{Flags: b[3], Length: binary.BigEndian.Uint32(b[4:HeaderSize])}, nil
}

// Frame 是一个逻辑帧。
type Frame struct {
	Type     Type
	StreamID uint32
	Payload  []byte
}

// Body 返回帧体的逻辑字节（Type|StreamID|Payload）。
func (f Frame) Body() []byte {
	b := make([]byte, BodyPrefixSize+len(f.Payload))
	b[0] = byte(f.Type)
	binary.BigEndian.PutUint32(b[1:BodyPrefixSize], f.StreamID)
	copy(b[BodyPrefixSize:], f.Payload)
	return b
}

// ParseBody 解析逻辑帧体。返回的 Frame.Payload 与入参 body 共享底层数组。
func ParseBody(body []byte) (Frame, error) {
	if len(body) < BodyPrefixSize {
		return Frame{}, fmt.Errorf("%w: body=%d", ErrShortFrame, len(body))
	}
	t := Type(body[0])
	if !t.Valid() {
		return Frame{}, fmt.Errorf("%w: 0x%02X", ErrUnknownType, body[0])
	}
	return Frame{
		Type:     t,
		StreamID: binary.BigEndian.Uint32(body[1:BodyPrefixSize]),
		Payload:  body[BodyPrefixSize:],
	}, nil
}

// ReadFrame 从 r 读取一个完整帧，返回已校验的明文头与帧体。
//
// maxBody 之上的 Length 会立即报错而不尝试读取，避免被伪造长度拖住。
func ReadFrame(r io.Reader, maxBody uint32) (Header, []byte, error) {
	var hb [HeaderSize]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return Header{}, nil, err
	}
	hdr, err := ParseHeader(hb[:])
	if err != nil {
		return Header{}, nil, err
	}
	if hdr.Length > maxBody {
		return hdr, nil, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, hdr.Length, maxBody)
	}
	body := make([]byte, hdr.Length)
	if _, err := io.ReadFull(r, body); err != nil {
		return hdr, nil, err
	}
	return hdr, body, nil
}

// MarshalPlain 按握手态编码（Body 明文）。
func MarshalPlain(f Frame) ([]byte, error) {
	body := f.Body()
	if len(body) > MaxFrameBody {
		return nil, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, len(body), MaxFrameBody)
	}
	hdr := Header{Length: uint32(len(body))}
	hb := hdr.Marshal()
	out := make([]byte, 0, HeaderSize+len(body))
	out = append(out, hb[:]...)
	out = append(out, body...)
	return out, nil
}

// UnmarshalPlainBody 解析握手态帧体（hdr 用于长度一致性检查）。
func UnmarshalPlainBody(hdr Header, body []byte) (Frame, error) {
	if int(hdr.Length) != len(body) {
		return Frame{}, fmt.Errorf("%w: 头声明 %d，实际 %d", ErrBadLength, hdr.Length, len(body))
	}
	return ParseBody(body)
}

// UnmarshalPlain 解析一个完整的握手态帧。
func UnmarshalPlain(raw []byte) (Frame, error) {
	hdr, err := ParseHeader(raw)
	if err != nil {
		return Frame{}, err
	}
	return UnmarshalPlainBody(hdr, raw[HeaderSize:])
}

// MarshalSecure 按安全态编码：Body 为 AEAD 密文，明文头作为 AAD。
func MarshalSecure(c Cipher, n *NonceCounter, f Frame) ([]byte, error) {
	plain := f.Body()
	bodyLen := len(plain) + c.Overhead()
	if bodyLen > MaxFrameBody {
		return nil, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, bodyLen, MaxFrameBody)
	}
	nonce, err := n.Next()
	if err != nil {
		return nil, err
	}
	// Length 必须先定下来才能把它写进 AAD。
	hdr := Header{Length: uint32(bodyLen)}
	hb := hdr.Marshal()
	body := c.Seal(nil, nonce, plain, hb[:])
	if len(body) != bodyLen {
		return nil, fmt.Errorf("protocol: 密文长度异常: %d != %d", len(body), bodyLen)
	}
	out := make([]byte, 0, HeaderSize+len(body))
	out = append(out, hb[:]...)
	out = append(out, body...)
	return out, nil
}

// UnmarshalSecure 解开一个安全态帧（body 为密文，hdr 为收到的明文头，用于重建 AAD）。
//
// 无论解密成功与否，nonce 计数器都会前进：发送方每发一帧就自增一次，
// 接收方必须保持同样的步调，否则后续帧的 nonce 会错位。
func UnmarshalSecure(c Cipher, n *NonceCounter, hdr Header, body []byte) (Frame, error) {
	if len(body) < c.Overhead()+BodyPrefixSize {
		return Frame{}, fmt.Errorf("%w: body=%d", ErrShortFrame, len(body))
	}
	if int(hdr.Length) != len(body) {
		return Frame{}, fmt.Errorf("%w: 头声明 %d，实际 %d", ErrBadLength, hdr.Length, len(body))
	}
	nonce, err := n.Next()
	if err != nil {
		return Frame{}, err
	}
	hb := hdr.Marshal()
	plain, err := c.Open(nil, nonce, body, hb[:])
	if err != nil {
		return Frame{}, fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return ParseBody(plain)
}
