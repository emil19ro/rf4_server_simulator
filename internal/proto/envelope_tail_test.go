package proto

import (
	"bytes"
	"testing"
)

// tailRequest 构造 marker=-1 的请求信封：main/sub + body + tail；tail<0 表示不带尾字节。
func tailRequest(callID uint32, main, sub uint8, body []byte, tail int) []byte {
	out := append([]byte{0x01}, PackI32(-1)...)
	out = append(out, PackU32(callID)...)
	out = append(out, main, sub)
	out = append(out, body...)
	if tail >= 0 {
		out = append(out, byte(tail))
	}
	return out
}

// argBody args("524") 空列表，7 字节，末字节是 0x00（与 expect_response 同值，只能靠尾字节位置区分）。
func argBody() []byte { return []byte{0x03, 0x03, '5', '2', '4', 0x00, 0x00} }

// oddBody 末字节 0x07，不属于 expect_response 取值域。
func oddBody() []byte { return []byte{0x03, 0x03, '5', '2', '4', 0x00, 0x07} }

// TestParseEnvelopeExpectTailDomain 只有 0x00/0x01 才被认作 expect_response 尾字节；
// 其他取值必须原样留在 body 里，否则真实请求会被截掉最后一个字节。
func TestParseEnvelopeExpectTailDomain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    []byte
		tail    int // <0 表示不带尾字节
		want    int // Expect 期望值，<0 表示应为 nil
		wantHas bool
	}{
		{"尾字节0x00", argBody(), 0x00, 0, true},
		{"尾字节0x01", argBody(), 0x01, 1, true},
		{"未识别尾字节0x7f", argBody(), 0x7f, -1, false},
		{"未识别尾字节0x02", oddBody(), -1, -1, false},
		{"空body带尾字节0x00", nil, 0x00, 0, true},
		{"空body带未识别尾字节0x7f", nil, 0x7f, -1, false},
		{"空body无尾字节", nil, -1, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := tailRequest(9, 21, 2, tc.body, tc.tail)
			env, ok := ParseEnvelope(payload)
			if !ok {
				t.Fatalf("解析失败: %x", payload)
			}
			wantBody := append([]byte{}, tc.body...)
			if tc.want < 0 {
				if tc.tail >= 0 {
					wantBody = append(wantBody, byte(tc.tail))
				}
				if env.Expect != nil {
					t.Fatalf("未识别取值 %#x 不应成为 Expect，实得 %#x", tc.tail, *env.Expect)
				}
			} else if env.Expect == nil || int(*env.Expect) != tc.want {
				t.Fatalf("Expect = %v, want %#x", env.Expect, tc.want)
			}
			if env.HasTail != tc.wantHas {
				t.Fatalf("HasTail = %v, want %v", env.HasTail, tc.wantHas)
			}
			if env.HasTail != (env.Expect != nil) {
				t.Fatalf("HasTail 与 Expect 不一致: %+v", env)
			}
			if !bytes.Equal(env.Body, wantBody) {
				t.Fatalf("Body = %x, want %x", env.Body, wantBody)
			}
			if env.Marker != -1 || env.CallID != 9 || env.Main != 21 || env.Sub != 2 {
				t.Fatalf("信封字段不符: %+v", env)
			}
		})
	}
}

// TestParseEnvelopeResponseKeepsBody marker=-2 的响应没有 expect_response 尾字节，
// 末字节即使是 0x00/0x01 也必须保留。
func TestParseEnvelopeResponseKeepsBody(t *testing.T) {
	for _, last := range []byte{0x00, 0x01} {
		payload := BuildResponseEnvelope(42, []byte{last})
		env, ok := ParseEnvelope(payload)
		if !ok || env.Marker != -2 || env.CallID != 42 || env.HasTail || env.Expect != nil {
			t.Fatalf("响应信封被误判: %+v", env)
		}
		if !bytes.Equal(env.Body, []byte{last}) {
			t.Fatalf("响应 body 丢了末字节: %x", env.Body)
		}
	}
}

// TestParseEnvelopeShortRequest marker=-1 但长度不足 11 时不越界、不识别尾字节。
func TestParseEnvelopeShortRequest(t *testing.T) {
	payload := append([]byte{0x01}, PackI32(-1)...)
	payload = append(payload, PackU32(5)...)
	env, ok := ParseEnvelope(payload)
	if !ok || env.HasTail || env.Expect != nil || len(env.Body) != 0 {
		t.Fatalf("长度 9 的请求信封解析不符: %+v", env)
	}
	env, ok = ParseEnvelope(append(append([]byte{}, payload...), 0x00))
	if !ok || env.HasTail || env.Expect != nil || env.Main != 0 || env.Sub != 0 || len(env.Body) != 1 {
		t.Fatalf("长度 10 的请求信封解析不符: %+v", env)
	}
}
