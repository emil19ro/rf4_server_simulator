package proto

import (
	"fmt"
	"strings"
	"testing"
)

// countLimit 是 u16 计数前缀能表达的最大值 + 1：达到它就必须报错。
const countLimit = 1 << 16

// countCase 描述一个「先写定宽计数、再写元素」的容器分支。
type countCase struct {
	name  string
	build func(n int) *Value
}

// countCases 覆盖 encode.go 里全部 u16 计数分支。
func countCases() []countCase {
	code := "524"
	return []countCase{
		{
			name: "args",
			build: func(n int) *Value {
				return &Value{Kind: KindArgs, Code: &code, Items: make([]*Value, n)}
			},
		},
		{
			name: "array",
			build: func(n int) *Value {
				items := make([]*Value, n)
				for i := range items {
					items[i] = &Value{Kind: KindBytes, IsNull: true}
				}
				return &Value{Kind: KindArray, Items: items}
			},
		},
		{
			name: "u32arr",
			build: func(n int) *Value {
				items := make([]*Value, n)
				for i := range items {
					items[i] = &Value{Kind: KindU32, Num: uint64(i)}
				}
				return &Value{Kind: KindU32Arr, Items: items}
			},
		},
		{
			name: "guidarr",
			build: func(n int) *Value {
				items := make([]*Value, n)
				for i := range items {
					items[i] = &Value{Kind: KindGuid, Hex: fmt.Sprintf("%032x", i)}
				}
				return &Value{Kind: KindGuidArr, Items: items}
			},
		},
		{
			name: "typed_u32arr",
			build: func(n int) *Value {
				items := make([]*Value, n)
				for i := range items {
					items[i] = &Value{Kind: KindU32, Num: uint64(i)}
				}
				return &Value{Kind: KindTypedU32Arr, TypeName: "myenum", Items: items}
			},
		},
		{
			name: "typedarr",
			build: func(n int) *Value {
				return &Value{Kind: KindTypedArr, TypeName: "System.String", Items: make([]*Value, n)}
			},
		},
	}
}

// TestEncodeCountOverflow 元素数达到 65536 时长度前缀会被 u16 静默截断，
// 编码必须直接报错，且不返回任何半截字节。
func TestEncodeCountOverflow(t *testing.T) {
	table := NewOpsTable(map[int32]string{}, map[string][]string{})
	for _, c := range countCases() {
		t.Run(c.name, func(t *testing.T) {
			raw, err := EncodeValue(c.build(countLimit), table)
			if err == nil {
				t.Fatalf("%d 个元素应报错，实际编码出 %d 字节", countLimit, len(raw))
			}
			if raw != nil {
				t.Fatalf("%d 个元素报错时不得返回字节，实得 %d 字节", countLimit, len(raw))
			}
			if !strings.Contains(err.Error(), "too many entries") {
				t.Fatalf("错误信息应包含 too many entries，实得: %v", err)
			}
		})
	}
}

// TestEncodeCountBoundary 65535 个元素是 u16 计数上限内的合法边界，
// 必须正常编码并能原样解码。
func TestEncodeCountBoundary(t *testing.T) {
	table := NewOpsTable(map[int32]string{}, map[string][]string{})
	for _, c := range countCases() {
		t.Run(c.name, func(t *testing.T) {
			raw, err := EncodeValue(c.build(countLimit-1), table)
			if err != nil {
				t.Fatalf("%d 个元素（边界）应正常编码: %v", countLimit-1, err)
			}
			if len(raw) == 0 {
				t.Fatalf("%d 个元素编码结果为空", countLimit-1)
			}
			if _, _, err := DecodeValue(raw, 0, table); err != nil {
				t.Fatalf("%d 个元素编码结果无法解码: %v", countLimit-1, err)
			}
		})
	}
}

// TestEncodeDictCountOverflow dict 的计数前缀是 u8，超过 255 个键值对
// 同样会被静默截断，必须在写入任何字节之前报错。
func TestEncodeDictCountOverflow(t *testing.T) {
	table := NewOpsTable(map[int32]string{}, map[string][]string{})
	key := "k"
	pairs := make([]Pair, 0x100)
	for i := range pairs {
		pairs[i] = Pair{K: &key, V: Null()}
	}
	raw, err := EncodeValue(&Value{Kind: KindDict, TypeName: "mydict", Pairs: pairs}, table)
	if err == nil {
		t.Fatalf("256 个键值对应报错，实际编码出 %d 字节", len(raw))
	}
	if raw != nil {
		t.Fatalf("256 个键值对报错时不得返回字节，实得 %d 字节", len(raw))
	}
	if !strings.Contains(err.Error(), "too many entries") {
		t.Fatalf("错误信息应包含 too many entries，实得: %v", err)
	}
}

// TestEncodeDictCountBoundary 255 个键值对仍在 u8 计数上限内，必须正常编码。
func TestEncodeDictCountBoundary(t *testing.T) {
	table := NewOpsTable(map[int32]string{}, map[string][]string{})
	key := "k"
	pairs := make([]Pair, 0xFF)
	for i := range pairs {
		pairs[i] = Pair{K: &key, V: Null()}
	}
	raw, err := EncodeValue(&Value{Kind: KindDict, TypeName: "mydict", Pairs: pairs}, table)
	if err != nil {
		t.Fatalf("255 个键值对（边界）应正常编码: %v", err)
	}
	if len(raw) != 2+len("mydict")+1+0xFF*3 {
		t.Fatalf("边界编码长度异常: %d", len(raw))
	}
}
