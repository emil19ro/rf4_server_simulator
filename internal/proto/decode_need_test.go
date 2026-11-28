package proto

import "testing"

// TestNeedRejectsNegativeLen 校验 need 拒绝负长度。
//
// 长度取自协议里的 u32 字段，32 位平台上最高位置位转换后为负数，
// 必须按 EOF 拒绝，否则后续切片上界为负会 panic。
func TestNeedRejectsNegativeLen(t *testing.T) {
	cases := []struct {
		name string
		n    int
	}{
		{name: "负一", n: -1},
		{name: "u32 最高位置位截断", n: -2147483647},
		{name: "int 最小值", n: -1 << 31},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &reader{data: []byte{1, 2, 3, 4}, pos: 1}
			err := r.need(tc.n, "test")
			if err == nil {
				t.Fatalf("need(%d) 期望返回错误，实际返回 nil（位置 %d，数据长度 %d）", tc.n, r.pos, len(r.data))
			}
			if r.pos != 1 {
				t.Fatalf("need(%d) 失败后位置应保持 1，实际 %d", tc.n, r.pos)
			}
		})
	}
}

// TestBytesRejectsNegativeLen 校验唯一直接切片的访问器 bytes 对负长度返回错误，
// 数据为 nil 且位置不动。
func TestBytesRejectsNegativeLen(t *testing.T) {
	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("bytes(-4) 期望返回错误，实际 panic: %v", v)
		}
	}()
	r := &reader{data: []byte{1, 2, 3, 4}, pos: 2}
	got, err := r.bytes(-4, "test")
	if err == nil {
		t.Fatalf("bytes(-4) 期望返回错误，实际返回 nil 错误，数据 %x", got)
	}
	if got != nil {
		t.Fatalf("bytes(-4) 失败时应返回 nil 数据，实际 %x", got)
	}
	if r.pos != 2 {
		t.Fatalf("bytes(-4) 失败后位置应保持 2，实际 %d", r.pos)
	}
}

// TestNeedBoundary 校验 need 的边界：0 与恰好等于剩余长度放行，超出剩余量报错，
// 三种情况都不移动位置。
func TestNeedBoundary(t *testing.T) {
	data := []byte{1, 2, 3, 4}
	cases := []struct {
		name    string
		pos     int
		n       int
		wantErr bool
	}{
		{name: "零长度在末尾放行", pos: 4, n: 0, wantErr: false},
		{name: "零长度在开头放行", pos: 0, n: 0, wantErr: false},
		{name: "恰好等于剩余长度", pos: 1, n: 3, wantErr: false},
		{name: "超出剩余一字节", pos: 1, n: 4, wantErr: true},
		{name: "末尾读一字节", pos: 4, n: 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &reader{data: data, pos: tc.pos}
			err := r.need(tc.n, "test")
			if tc.wantErr && err == nil {
				t.Fatalf("need(%d)@pos=%d 期望返回 EOF 错误，实际返回 nil（剩余 %d 字节）", tc.n, tc.pos, len(data)-tc.pos)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("need(%d)@pos=%d 期望放行，实际返回错误: %v", tc.n, tc.pos, err)
			}
			if r.pos != tc.pos {
				t.Fatalf("need(%d)@pos=%d 不应移动位置，实际 %d", tc.n, tc.pos, r.pos)
			}
		})
	}
}

// TestDecodeValueRejectsHugeStrLen 走完整解码路径：u32 长度最高位置位时，
// 32 位平台 int(ln) 为负、64 位平台为超大正数，两者都必须返回错误而不是 panic。
func TestDecodeValueRejectsHugeStrLen(t *testing.T) {
	for _, ln := range []uint32{0x80000001, 0xFFFFFFFC, 0xFFFFFFFF} {
		raw := append([]byte{0x16}, PackU32(ln)...) // 0x16 = u32 长度字符串
		raw = append(raw, 'a', 'b', 'c', 'd')
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("长度 0x%08X 解码期望返回错误，实际 panic: %v", ln, v)
				}
			}()
			v, n, err := DecodeValue(raw, 0, nil)
			if err == nil {
				t.Fatalf("长度 0x%08X 期望返回错误，实际解码成功: %+v，结束位置 %d", ln, v, n)
			}
		}()
	}
}
