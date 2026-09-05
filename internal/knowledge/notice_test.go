package knowledge

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// Unit tests for notice.go (007-document-processing-notice).
//
// 这一列没有数据库 CHECK 约束——"逗号分隔的升序正整数"不是 MySQL 能便宜表达的
// 结构（见 migration 000015 的注释）。**不变量因此全部由这里保证**，这不是
// "约束更弱"，是把约束放在了唯一能便宜表达它的地方。

func TestEncodeUnextractedPagesEmptyIsNullNotEmptyString(t *testing.T) {
	for _, in := range [][]int{nil, {}, {0}, {-1, 0}} {
		got := encodePageList(in)
		if got.Valid {
			t.Fatalf("encodePageList(%v) = %q（Valid），应当是 NULL——"+
				"「没有缺页」和「长度为 0 的列表」是同一件事，只能有一种表示", in, got.String)
		}
	}
}

// TestEncodeUnextractedPagesNormalises 锁定 N2/N3：编码时显式排序、去重、
// 过滤非正值。⭐ 排序不是装饰——textLayerCoverage 目前恰好按页序产出，但那是
// 调用方的实现细节；依赖它就是把存储顺序从「值的性质」变成「调用链的性质」，
// 上游哪天改了遍历方式，同一组页码就会存出两种形态（宪法第 V 条）。
func TestEncodeUnextractedPagesNormalises(t *testing.T) {
	cases := []struct {
		name string
		in   []int
		want string
	}{
		{"已经有序", []int{2, 4}, "2,4"},
		{"乱序 -> 升序", []int{4, 2, 17}, "2,4,17"},
		{"重复 -> 去重", []int{4, 2, 4, 2}, "2,4"},
		{"非正值被过滤", []int{0, -3, 5, 1}, "1,5"},
		{"单页", []int{7}, "7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encodePageList(tc.in)
			if !got.Valid || got.String != tc.want {
				t.Fatalf("encodePageList(%v) = %v/%q，应当是 %q", tc.in, got.Valid, got.String, tc.want)
			}
		})
	}
}

// TestEncodeUnextractedPagesIsDeterministic：同一组页码的**任意排列**必须编码
// 成同一个字符串。这是上面那条排序要求的行为形态。
func TestEncodeUnextractedPagesIsDeterministic(t *testing.T) {
	want := encodePageList([]int{2, 4, 17, 33})
	for _, perm := range [][]int{
		{4, 2, 33, 17},
		{33, 17, 4, 2},
		{17, 33, 2, 4},
		{2, 17, 4, 33, 4, 2}, // 带重复
	} {
		if got := encodePageList(perm); got.String != want.String {
			t.Fatalf("encodePageList(%v) = %q，与规范形态 %q 不同——编码依赖了输入顺序",
				perm, got.String, want.String)
		}
	}
}

func TestUnextractedPagesRoundTrip(t *testing.T) {
	cases := [][]int{
		{2, 4},
		{1},
		{4, 2, 17}, // 乱序
		{3, 3, 3},  // 全重复
		{1, 2, 3, 5, 8, 13},
	}
	for _, in := range cases {
		t.Run(fmt.Sprint(in), func(t *testing.T) {
			normalized := decodePageList(encodePageList(in))
			// 往返之后必须等于规范化形态：升序、去重。
			again := decodePageList(encodePageList(normalized))
			if len(again) != len(normalized) {
				t.Fatalf("往返不稳定：%v -> %v -> %v", in, normalized, again)
			}
			for i := range again {
				if again[i] != normalized[i] {
					t.Fatalf("往返不稳定：%v -> %v -> %v", in, normalized, again)
				}
			}
			for i := 1; i < len(normalized); i++ {
				if normalized[i] <= normalized[i-1] {
					t.Fatalf("解码结果不是严格升序：%v", normalized)
				}
			}
		})
	}
}

func TestDecodeUnextractedPagesNullAndEmpty(t *testing.T) {
	for _, in := range []sql.NullString{
		{},
		{String: "", Valid: true},
		{String: "   ", Valid: true},
	} {
		if got := decodePageList(in); got != nil {
			t.Fatalf("decodePageList(%+v) = %v，应当是 nil", in, got)
		}
	}
}

// TestDecodeUnextractedPagesCorruptValueDegradesToNoNotice 锁定 N5。
//
// ⚠️ 这一条是有意的取舍，不是疏忽：这是一列**只用于展示**的数据。让一个无法
// 解析的历史值把解码变成错误，就是把一个纯装饰性的数据问题升级成「整个文档
// 列表打不开」——而文档列表恰恰是用户看到任何东西的唯一入口。静默地不显示
// 提示是轻的那种失败，这里刻意选它。
func TestDecodeUnextractedPagesCorruptValueDegradesToNoNotice(t *testing.T) {
	for _, raw := range []string{
		"not-a-number",
		"2,abc,4", // 部分可解析：坏的丢掉，好的留下
		",,,",
		"0,-1", // 全是非法页码
		"2;4",  // 分隔符不对
	} {
		got := decodePageList(sql.NullString{String: raw, Valid: true})
		for _, p := range got {
			if p < 1 {
				t.Fatalf("decodePageList(%q) 返回了非正页码 %v", raw, got)
			}
		}
	}
	// "2,abc,4" 里合法的部分要留下——坏一个字段不该丢掉整条信息。
	if got := decodePageList(sql.NullString{String: "2,abc,4", Valid: true}); len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Fatalf("decodePageList(\"2,abc,4\") = %v，应当是 [2 4]", got)
	}
	// 完全无法解析的值降级为「无提示」，而不是报错。
	if got := decodePageList(sql.NullString{String: "not-a-number", Valid: true}); got != nil {
		t.Fatalf("完全坏掉的值应当降级为 nil，实际 %v", got)
	}
}

// TestDecodeUnextractedPagesRenormalisesDiskValue：磁盘上的值可能是更早、更糟
// 的版本写的（乱序、重复）。解码时重新规范化，而不是信任磁盘。
func TestDecodeUnextractedPagesRenormalisesDiskValue(t *testing.T) {
	got := decodePageList(sql.NullString{String: "17,2,4,2", Valid: true})
	want := []int{2, 4, 17}
	if len(got) != len(want) {
		t.Fatalf("decodePageList = %v，应当是 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("decodePageList = %v，应当是 %v", got, want)
		}
	}
}

// TestUnextractedPagesLargeDocument：一份千页文档几乎全是扫描页时，编码结果
// 仍要能放进 TEXT（64KB）且往返无损。存储层**不截断**——截断会永久丢掉真实
// 数量，而截断本该是展示层的事。
func TestUnextractedPagesLargeDocument(t *testing.T) {
	pages := make([]int, 0, 1000)
	for i := 1; i <= 1000; i++ {
		pages = append(pages, i)
	}
	enc := encodePageList(pages)
	if !enc.Valid {
		t.Fatal("千页文档编码成了 NULL")
	}
	if len(enc.String) > 60000 {
		t.Fatalf("编码长度 %d 接近 TEXT 上限，需要重新考虑存储形态", len(enc.String))
	}
	if got := decodePageList(enc); len(got) != 1000 {
		t.Fatalf("往返后剩 %d 页，应当是 1000——存储层不得截断", len(got))
	}
	if !strings.HasPrefix(enc.String, "1,2,3,") {
		t.Fatalf("编码结果开头 = %q，应当是升序", enc.String[:20])
	}
}
