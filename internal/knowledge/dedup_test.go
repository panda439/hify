package knowledge

import (
	"reflect"
	"testing"
)

func dedupRC(id string, score float64, content string) RetrievedChunk {
	return RetrievedChunk{Chunk: Chunk{ID: id, Content: content}, Score: score}
}

// --- normalizeContentForDedup ---

// 二. CRLF/LF、首尾空白、行尾空格差异能够被识别为同一份内容。
func TestNormalizeContentForDedupUnifiesCRLFAndTrimsWhitespace(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"CRLF vs LF", "line one\r\nline two", "line one\nline two"},
		{"leading/trailing whitespace", "  hello world  \n", "hello world"},
		{"trailing spaces per line", "hello  \nworld\t\n", "hello\nworld"},
		{"mixed: CRLF + trailing spaces + outer whitespace", "\r\n  first line  \r\nsecond line\t\r\n  ", "first line\nsecond line"},
	}
	for _, tc := range cases {
		na, nb := normalizeContentForDedup(tc.a), normalizeContentForDedup(tc.b)
		if na != nb {
			t.Fatalf("%s: normalize(%q) = %q, normalize(%q) = %q, want equal", tc.name, tc.a, na, tc.b, nb)
		}
	}
}

// 待修复项 2（审核修复）: 规范化只允许统一 CRLF -> LF；单独的 CR（不是
// CRLF 的一部分）绝不能被当作换行符处理，"a\rb" 和 "a\nb" 必须保持不同。
// 一个更早的版本额外把裸 CR 也替换成了 LF，这超出了"只统一 CRLF"这条阶段
// 约束允许的范围。
func TestNormalizeContentForDedupNeverFoldsLoneCRIntoLF(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"lone CR vs LF", "a\rb", "a\nb"},
		{"lone CR vs plain space", "line one\rline two", "line one line two"},
	}
	for _, tc := range cases {
		na, nb := normalizeContentForDedup(tc.a), normalizeContentForDedup(tc.b)
		if na == nb {
			t.Fatalf("%s: normalize(%q) = %q and normalize(%q) = %q must NOT be equal — a lone CR is not CRLF and must not be folded into LF", tc.name, tc.a, na, tc.b, nb)
		}
	}
	// A lone CR by itself is preserved as-is (only surrounding whitespace
	// trimming applies to it, same as any other non-newline rune) — it is
	// simply not treated as a line separator.
	if got, want := normalizeContentForDedup("a\rb"), "a\rb"; got != want {
		t.Fatalf("normalize(%q) = %q, want %q unchanged (lone CR is not a recognized line separator)", "a\rb", got, want)
	}
}

// 三. 大小写、标点、内部（行内）缩进的差异不能被误判为重复。
func TestNormalizeContentForDedupPreservesCaseAndPunctuationAndIndentation(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"case", "Hello World", "hello world"},
		{"punctuation", "wait, really?", "wait really"},
		{"internal indentation", "func f() {\n    return 1\n}", "func f() {\nreturn 1\n}"},
		{"internal double space", "a  b", "a b"},
	}
	for _, tc := range cases {
		na, nb := normalizeContentForDedup(tc.a), normalizeContentForDedup(tc.b)
		if na == nb {
			t.Fatalf("%s: normalize(%q) and normalize(%q) both = %q, want them to stay distinct (this is exact dedup, not fuzzy dedup)", tc.name, tc.a, tc.b, na)
		}
	}
}

func TestNormalizeContentForDedupEmptyString(t *testing.T) {
	if got := normalizeContentForDedup(""); got != "" {
		t.Fatalf("normalize(\"\") = %q, want empty", got)
	}
	if got := normalizeContentForDedup("   \r\n\t  "); got != "" {
		t.Fatalf("normalize(whitespace-only) = %q, want empty", got)
	}
}

// --- dedupExactContentChunks ---

// 一. 不同 ID、相同正文（规范化后）只保留最高排名（即输入序列中最靠前）
// 的那一条。
func TestDedupExactContentChunksKeepsHighestRankedDuplicate(t *testing.T) {
	in := []RetrievedChunk{
		dedupRC("high-rank", 0.9, "同一段正文内容"),
		dedupRC("low-rank", 0.5, "同一段正文内容"),
		dedupRC("unique", 0.4, "完全不同的内容"),
	}
	got, suppressed := dedupExactContentChunks(in)
	want := []string{"high-rank", "unique"}
	if ids := idsOf(got); !reflect.DeepEqual(ids, want) {
		t.Fatalf("got %v, want %v (the earlier/higher-ranked duplicate must survive, not the later one)", ids, want)
	}
	if suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1 (exactly one duplicate dropped)", suppressed)
	}
}

// 三（延伸）: 内容确实不同的候选一个都不能被误删.
func TestDedupExactContentChunksNoFalsePositiveForDifferentContent(t *testing.T) {
	in := []RetrievedChunk{
		dedupRC("a", 0.9, "Hello World"),
		dedupRC("b", 0.8, "hello world"),
		dedupRC("c", 0.7, "func f() {\n    return 1\n}"),
		dedupRC("d", 0.6, "func f() {\nreturn 1\n}"),
	}
	got, suppressed := dedupExactContentChunks(in)
	if len(got) != len(in) {
		t.Fatalf("got %d results %v, want all %d kept — case/indentation differences must never be treated as duplicates", len(got), idsOf(got), len(in))
	}
	if suppressed != 0 {
		t.Fatalf("suppressed = %d, want 0 (nothing here is a genuine duplicate)", suppressed)
	}
}

// 七. 保留下来的条目的 Content（原始未规范化文本）、Score、Citation 元数据
// （DocumentName/PageNumber/SectionTitle）、DocumentVersion、NeighborOf 都
// 必须原样保留，不能被去重逻辑改写或用规范化后的文本覆盖。
func TestDedupExactContentChunksPreservesOriginalFieldsOfKeptEntry(t *testing.T) {
	page := 3
	section := "1.2 Intro"
	kept := RetrievedChunk{
		Chunk: Chunk{
			ID: "keep", Content: "  Some Text.\r\n  ", DocumentName: "doc.pdf",
			PageNumber: &page, SectionTitle: &section, DocumentVersion: 7,
		},
		Score:      0.83,
		NeighborOf: "anchor-x",
	}
	dup := RetrievedChunk{Chunk: Chunk{ID: "drop", Content: "Some Text."}, Score: 0.1}

	got, suppressed := dedupExactContentChunks([]RetrievedChunk{kept, dup})
	if len(got) != 1 {
		t.Fatalf("got %d results %v, want exactly 1 (dup must be dropped)", len(got), idsOf(got))
	}
	if suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1", suppressed)
	}
	if got[0].Content != kept.Content {
		t.Fatalf("Content = %q, want the ORIGINAL un-normalized %q (normalization must never overwrite Content)", got[0].Content, kept.Content)
	}
	if got[0].Score != kept.Score {
		t.Fatalf("Score = %f, want %f", got[0].Score, kept.Score)
	}
	if got[0].DocumentName != kept.DocumentName || got[0].PageNumber != kept.PageNumber || got[0].SectionTitle != kept.SectionTitle {
		t.Fatalf("Citation metadata changed: got DocumentName=%q PageNumber=%v SectionTitle=%v", got[0].DocumentName, got[0].PageNumber, got[0].SectionTitle)
	}
	if got[0].DocumentVersion != kept.DocumentVersion {
		t.Fatalf("DocumentVersion = %d, want %d", got[0].DocumentVersion, kept.DocumentVersion)
	}
	if got[0].NeighborOf != kept.NeighborOf {
		t.Fatalf("NeighborOf = %q, want %q (must not be rewritten by dedup)", got[0].NeighborOf, kept.NeighborOf)
	}
}

func TestDedupExactContentChunksEmptyAndSingleInput(t *testing.T) {
	if got, suppressed := dedupExactContentChunks(nil); got != nil || suppressed != 0 {
		t.Fatalf("dedupExactContentChunks(nil) = %v, %d, want nil, 0", got, suppressed)
	}
	if got, suppressed := dedupExactContentChunks([]RetrievedChunk{}); len(got) != 0 || suppressed != 0 {
		t.Fatalf("dedupExactContentChunks(empty) = %v, %d, want empty, 0", got, suppressed)
	}
	single := []RetrievedChunk{dedupRC("only", 0.5, "content")}
	got, suppressed := dedupExactContentChunks(single)
	if !reflect.DeepEqual(idsOf(got), []string{"only"}) {
		t.Fatalf("got %v, want [only] unchanged", idsOf(got))
	}
	if suppressed != 0 {
		t.Fatalf("suppressed = %d, want 0", suppressed)
	}
}

// 一个 ID 只在 seen map 里出现一次并不代表内容判定用了 ID —
// 显式确认判重键完全来自内容，与 ID/Score 无关。
func TestDedupExactContentChunksKeyIsContentOnlyNotIDOrScore(t *testing.T) {
	in := []RetrievedChunk{
		dedupRC("id-1", 0.99, "duplicate text"),
		dedupRC("id-2", 0.01, "duplicate text"),
	}
	got, suppressed := dedupExactContentChunks(in)
	if len(got) != 1 || got[0].ID != "id-1" {
		t.Fatalf("got %v, want exactly [id-1] regardless of how different Score/ID are", idsOf(got))
	}
	if suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1", suppressed)
	}
}

// --- 待修复项 1（审核修复）: 空正文绝不参与去重 ---

// 两个不同 ID、正文均为空字符串的 chunk 必须全部保留，绝不能因为规范化后
// 都是 "" 就被折叠成一条。
func TestDedupExactContentChunksNeverCollapsesEmptyContent(t *testing.T) {
	in := []RetrievedChunk{
		dedupRC("empty-1", 0.9, ""),
		dedupRC("empty-2", 0.8, ""),
		dedupRC("real", 0.7, "真实内容"),
	}
	got, suppressed := dedupExactContentChunks(in)
	want := []string{"empty-1", "empty-2", "real"}
	if ids := idsOf(got); !reflect.DeepEqual(ids, want) {
		t.Fatalf("got %v, want %v — empty-content chunks must never suppress each other", ids, want)
	}
	if suppressed != 0 {
		t.Fatalf("suppressed = %d, want 0 (empty-content chunks are kept, not counted as duplicates)", suppressed)
	}
}

// 仅含空白字符（规范化后同样是 ""）的正文，规则和真正的空字符串一致。
func TestDedupExactContentChunksNeverCollapsesWhitespaceOnlyContent(t *testing.T) {
	in := []RetrievedChunk{
		dedupRC("ws-1", 0.9, "   \r\n  "),
		dedupRC("ws-2", 0.8, "\t\t"),
	}
	got, suppressed := dedupExactContentChunks(in)
	if len(got) != 2 {
		t.Fatalf("got %d results %v, want both kept", len(got), idsOf(got))
	}
	if suppressed != 0 {
		t.Fatalf("suppressed = %d, want 0", suppressed)
	}
}

// 空正文的 chunk 不会挡在真正重复内容的前面、干扰后续的正常去重判断。
func TestDedupExactContentChunksEmptyContentDoesNotInterfereWithRealDuplicates(t *testing.T) {
	in := []RetrievedChunk{
		dedupRC("empty", 0.95, ""),
		dedupRC("dup-high", 0.9, "重复内容"),
		dedupRC("dup-low", 0.5, "重复内容"),
	}
	got, suppressed := dedupExactContentChunks(in)
	want := []string{"empty", "dup-high"}
	if ids := idsOf(got); !reflect.DeepEqual(ids, want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
	if suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1 (only dup-low, the empty-content chunk never counts)", suppressed)
	}
}
