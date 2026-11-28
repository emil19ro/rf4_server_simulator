package proto

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Frame 一个完整 realtime 帧。13 字节明文头 + RC4 payload（本包不管加密）。
type Frame struct {
	Raw       []byte // 完整原始字节（头+payload）
	BodyLen   uint32
	FrameType uint8
	WireID    uint64
	Payload   []byte // 指向 Raw 的切片
}

// IsEmpty body_len==0：4 字节零帧（传输层保活）。
func (f *Frame) IsEmpty() bool { return f.BodyLen == 0 }

// maxFramePayload 单个帧 payload 的协议上限。数值与 internal/tunnel 的
// maxRecordPayload（32MB）对齐：帧载荷由网关经隧道转发，超过它的帧在隧道层
// 必然被拒；同时远低于 gateway 读循环 64MB 的缓冲上限。
const maxFramePayload = 32 << 20

// maxFrameBodyLen 是 body_len 的上限：9 字节头 + payload。
const maxFrameBodyLen = 9 + maxFramePayload

// ParseFrame 解析一个完整帧；数据不足返回 (nil, nil)；结构非法返回错误。
//
// 帧长规则：
//
//	body_len == 0 → 4 字节空帧
//	body_len == 1 → 5 字节，只带 type
//	body_len >= 9 → 4 + body_len 字节，payload = data[13:4+body_len]
//
// body_len > maxFrameBodyLen 视为非法帧：这种长度永远凑不齐，等下去只会让读
// 缓冲被一个永远满足不了的长度撑大。
func ParseFrame(data []byte) (*Frame, error) {
	if len(data) < 4 {
		return nil, nil
	}
	bodyLen := binary.LittleEndian.Uint32(data)
	switch {
	case bodyLen == 0:
		return &Frame{Raw: data[:4], BodyLen: 0, FrameType: 0, WireID: 0, Payload: nil}, nil
	case bodyLen == 1:
		if len(data) < 5 {
			return nil, nil
		}
		return &Frame{Raw: data[:5], BodyLen: 1, FrameType: data[4], WireID: 0, Payload: nil}, nil
	case bodyLen < 9, bodyLen > maxFrameBodyLen:
		return nil, errAt("frame", "parse", fmt.Sprintf("invalid body_len %d", bodyLen), 0)
	}
	// 长度运算必须离开 uint32 域：4+0xFFFFFFFF 在 uint32 里回绕成 3，会让越界帧
	// 通过下面的长度检查，随后的 data[5:13]、data[13:total] 直接 panic。
	total64 := uint64(bodyLen) + 4
	if total64 > uint64(len(data)) {
		return nil, nil
	}
	total := int(total64)
	return &Frame{
		Raw:       data[:total],
		BodyLen:   bodyLen,
		FrameType: data[4],
		WireID:    binary.LittleEndian.Uint64(data[5:13]),
		Payload:   data[13:total],
	}, nil
}

// BuildFrame [u32 body_len=9+len][u8 type][u64 wire_id] + payload。
//
// 调用方必须保证 len(payload) <= maxFramePayload：body_len 只有 uint32，超限帧
// ParseFrame 会拒收，写出去也没有对端会接受。
func BuildFrame(frameType uint8, wireID uint64, payload []byte) []byte {
	out := make([]byte, 0, 13+len(payload))
	out = append(out, PackU32(uint32(9+len(payload)))...)
	out = append(out, frameType)
	out = append(out, PackU64(wireID)...)
	return append(out, payload...)
}

// BuildAckFrame ACK 帧：type=1、无载荷（不消耗 RC4 keystream）、wire_id=被确认帧。
func BuildAckFrame(wireID uint64) []byte { return BuildFrame(1, wireID, nil) }

// BuildZeroFrame 4 字节零帧（传输层保活）。
func BuildZeroFrame() []byte { return PackU32(0) }

// TakeCompleteFrames 从缓冲区头部连续取出完整帧，返回消费掉的字节数。
// frames 为 nil 时表示缓冲区不足一帧；遇到不完整帧即停止，残余留给下次。
// ParseFrame 成功时 Raw 至少 4 字节，rest 每轮必然前移，不会空转。
func TakeCompleteFrames(buffer []byte) (frames []*Frame, consumed int, err error) {
	rest := buffer
	for {
		f, err := ParseFrame(rest)
		if err != nil {
			return frames, consumed, err
		}
		if f == nil {
			return frames, consumed, nil
		}
		frames = append(frames, f)
		rest = rest[len(f.Raw):]
		consumed += len(f.Raw)
	}
}

// ---- 握手原语 ----

// GreetingUUID 是客户端校验的服务端问候值；值错误时客户端会停留在未连接状态。
const GreetingUUID = "DAB12FBC-828D-4E00-B222-FB0D6EE7BF23"

// AuthMagic 认证包首字节（客户端 TcpStartMessage 固定前缀）。
const AuthMagic = 0x01

// TryParseAuthPacket 认证握手包：`01 <重连标记> + u32 len + token`。
// 新协议 token 含 >=3 个 '|'；旧版 CN 兼容口使用 32/48 位十六进制 token。
// 返回 (token, consumed, true)；数据不足/不匹配 → false。
//
// 第二字节是重连标记：首连为 0x00，重连为 0x01，两者都必须接受。
func TryParseAuthPacket(data []byte) (token string, consumed int, ok bool) {
	if len(data) < 6 || data[0] != AuthMagic {
		return "", 0, false
	}
	tokenLen := binary.LittleEndian.Uint32(data[2:6])
	total := int(6 + tokenLen)
	if tokenLen == 0 || len(data) < total {
		return "", 0, false
	}
	token = string(data[6:total])
	if !validAuthToken(token) {
		return "", 0, false
	}
	return token, total, true
}

// validAuthToken 同时接受作者协议的四段 token 与旧版 CN 的十六进制 token。
func validAuthToken(token string) bool {
	if len(token) == 0 || len(token) > 4096 {
		return false
	}
	pipes := 0
	for i := 0; i < len(token); i++ {
		if token[i] < 0x21 || token[i] > 0x7e {
			return false
		}
		if token[i] == '|' {
			pipes++
		}
	}
	if pipes >= 3 {
		return true
	}
	if len(token) != 32 && len(token) != 48 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

// AuthPacketReconnect 第二字节 == 1 → 客户端重连握手。
func AuthPacketReconnect(data []byte) bool {
	return len(data) >= 2 && data[0] == AuthMagic && data[1] == 0x01
}
