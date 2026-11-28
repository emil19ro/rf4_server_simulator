package proto

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestTryParseAuthPacketAcceptsLegacyAndAuthorTokens(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{name: "CN 32hex", token: "e28f0bc4bd956205b2636faa3e6d50bb"},
		{name: "CN 48hex", token: "59112aad24b1f0b5f9ad80e993c772353b33d2d181339e89"},
		{name: "author 4-part", token: "abcd|1|7247281|payload"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			packet := append([]byte{AuthMagic, 0x00}, PackU32(uint32(len(tc.token)))...)
			packet = append(packet, tc.token...)
			got, consumed, ok := TryParseAuthPacket(packet)
			if !ok || consumed != len(packet) || got != tc.token {
				t.Fatalf("解析失败: token=%q consumed=%d ok=%v", got, consumed, ok)
			}
		})
	}
}

func TestTryParseAuthPacketRejectsUnknownTokenFormat(t *testing.T) {
	for _, token := range []string{"not-a-token", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"} {
		packet := append([]byte{AuthMagic, 0x00}, PackU32(uint32(len(token)))...)
		packet = append(packet, token...)
		if _, _, ok := TryParseAuthPacket(packet); ok {
			t.Fatalf("不应接受 token %q", token)
		}
	}
}

// frameHeader 构造 13 字节明文头：body_len + type + wire_id。
func frameHeader(bodyLen uint32, frameType uint8) []byte {
	head := append(PackU32(bodyLen), frameType)
	return append(head, PackU64(0)...)
}

// 回绕与超限的 body_len 必须报错，绝不能解析出越界帧。
func TestParseFrameRejectsInvalidBodyLen(t *testing.T) {
	bodyLens := []uint32{0xFFFFFFFF, 0xFFFFFFFC, 0x80000000, maxFrameBodyLen + 1}
	for _, bodyLen := range bodyLens {
		for _, size := range []int{4, 8, 13} {
			data := make([]byte, size)
			copy(data, PackU32(bodyLen))
			f, err := ParseFrame(data)
			if f != nil || err == nil {
				t.Fatalf("body_len=%#x len(data)=%d: 期望 error，得到 frame=%v err=%v", bodyLen, size, f, err)
			}
		}
	}
}

// body_len 上限的边界：正好等于上限时数据不足应当等待；超一字节直接报错。
func TestParseFrameBodyLenLimitBoundary(t *testing.T) {
	if f, err := ParseFrame(frameHeader(maxFrameBodyLen, 0)); f != nil || err != nil {
		t.Fatalf("body_len=max 数据不足应返回 (nil, nil)，得到 frame=%v err=%v", f, err)
	}
	if _, err := ParseFrame(frameHeader(maxFrameBodyLen+1, 0)); err == nil {
		t.Fatalf("body_len=max+1 应报错")
	}
}

// 正好一帧能解析，少一字节返回 (nil, nil)，多余字节留给下一帧。
func TestParseFrameExactLengthBoundary(t *testing.T) {
	payload := []byte("frame-payload")
	bodyLen := uint32(9 + len(payload))
	data := append(frameHeader(bodyLen, 0), payload...)

	f, err := ParseFrame(data)
	if err != nil || f == nil {
		t.Fatalf("完整帧解析失败: frame=%v err=%v", f, err)
	}
	if f.BodyLen != bodyLen || string(f.Payload) != string(payload) || len(f.Raw) != len(data) {
		t.Fatalf("完整帧字段不符: %+v", f)
	}
	if f, err := ParseFrame(data[:len(data)-1]); f != nil || err != nil {
		t.Fatalf("少一字节应返回 (nil, nil)，得到 frame=%v err=%v", f, err)
	}
	extra := append(append([]byte{}, data...), 0xFF)
	if f, err = ParseFrame(extra); err != nil || f == nil || len(f.Raw) != len(data) {
		t.Fatalf("多余字节不应并入本帧: frame=%+v err=%v", f, err)
	}
}

// 畸形帧必须立即返回错误，不能原地空转。
func TestTakeCompleteFramesStopsOnInvalidBodyLen(t *testing.T) {
	buf := append(PackU32(0xFFFFFFFF), 1, 2, 3, 4)
	type result struct {
		frames   []*Frame
		consumed int
		err      error
	}
	done := make(chan result, 1)
	go func() {
		frames, consumed, err := TakeCompleteFrames(buf)
		done <- result{frames, consumed, err}
	}()
	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("畸形帧应返回 error")
		}
		if len(got.frames) != 0 || got.consumed != 0 {
			t.Fatalf("畸形帧不应消费字节: frames=%d consumed=%d", len(got.frames), got.consumed)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("TakeCompleteFrames 在畸形帧上没有返回")
	}
}

// 连续完整帧 + 尾部残帧：只消费完整帧。
func TestTakeCompleteFramesPartialTail(t *testing.T) {
	one := BuildFrame(0, 1, []byte("abc"))
	two := BuildFrame(0, 2, []byte("defg"))
	buf := append(append(append([]byte{}, one...), two...), one[:len(one)-1]...)
	frames, consumed, err := TakeCompleteFrames(buf)
	if err != nil {
		t.Fatalf("TakeCompleteFrames: %v", err)
	}
	if len(frames) != 2 || consumed != len(one)+len(two) {
		t.Fatalf("got %d frames, consumed %d", len(frames), consumed)
	}
}

// BuildFrame 与 ParseFrame 对称：构造出的 body_len 恰为 9+len(payload)。
func TestBuildFrameRoundTrip(t *testing.T) {
	payload := []byte("round-trip")
	data := BuildFrame(0, 0x1122334455667788, payload)
	if got := binary.LittleEndian.Uint32(data); got != uint32(9+len(payload)) {
		t.Fatalf("body_len=%d", got)
	}
	f, err := ParseFrame(data)
	if err != nil || f == nil {
		t.Fatalf("ParseFrame: frame=%v err=%v", f, err)
	}
	if f.FrameType != 0 || f.WireID != 0x1122334455667788 || string(f.Payload) != string(payload) {
		t.Fatalf("round trip 字段不符: %+v", f)
	}
}
