package knowledge

import (
	"fmt"
	"strings"
	"testing"
)

// Characterization tests for chunkText — 链路 2（文档入库流水线）的分块
// 环节。按 rune 而非 byte 切分（中文内容 byte 切会把多字节字符切半），
// 步长 = size - overlap。改坏的表现是 chunk 内容乱码或流水线死循环。

func TestChunkTextBasic(t *testing.T) {
	got := chunkText("一二三四五六七", 3, 1)
	want := []string{"一二三", "三四五", "五六七"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestChunkTextRunesNotBytes(t *testing.T) {
	// 每个中文字符 3 字节；若按 byte 切分，size=2 会产出乱码半字符。
	got := chunkText("中文", 1, 0)
	if len(got) != 2 || got[0] != "中" || got[1] != "文" {
		t.Fatalf("rune-based split broken: %v", got)
	}
}

func TestChunkTextEdgeCases(t *testing.T) {
	cases := []struct {
		name          string
		text          string
		size, overlap int
		want          int // 期望 chunk 数；-1 表示只要不 panic/不超时
	}{
		{"empty text", "", 10, 2, 0},
		{"whitespace-only chunks skipped", "   ", 2, 0, 0},
		{"text shorter than size", "abc", 10, 2, 1},
		{"zero size falls back to default 500", strings.Repeat("a", 1200), 0, 0, 3},
		{"overlap >= size falls back to no overlap", "abcdef", 2, 5, 3},
		{"negative overlap falls back to no overlap", "abcd", 2, -1, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkText(tc.text, tc.size, tc.overlap)
			if len(got) != tc.want {
				t.Fatalf("chunk count = %d (%v), want %d", len(got), got, tc.want)
			}
		})
	}
}

func TestChunkTextTrimsButKeepsInterior(t *testing.T) {
	got := chunkText("  ab  ", 6, 0)
	if len(got) != 1 || got[0] != "ab" {
		t.Fatalf("expected single trimmed chunk, got %v", got)
	}
}

// Characterization tests for batchStrings — ProcessDocument's embed
// batching (链路 2). Broken batching either drops/duplicates pieces
// across batch boundaries or produces a batch bigger than embedBatchSize.

func TestBatchStringsSplitsEvenly(t *testing.T) {
	items := []string{"a", "b", "c", "d"}
	got := batchStrings(items, 2)
	want := [][]string{{"a", "b"}, {"c", "d"}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if strings.Join(got[i], ",") != strings.Join(want[i], ",") {
			t.Fatalf("batch %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestBatchStringsLastBatchIsRemainder(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	got := batchStrings(items, 2)
	if len(got) != 3 {
		t.Fatalf("got %d batches %v, want 3", len(got), got)
	}
	if len(got[2]) != 1 || got[2][0] != "e" {
		t.Fatalf("last batch = %v, want [e]", got[2])
	}
}

func TestBatchStringsPreservesOrderAndTotalCount(t *testing.T) {
	items := make([]string, 100)
	for i := range items {
		items[i] = strings.Repeat("x", i+1) // 每个元素内容不同，方便按顺序核对
	}
	got := batchStrings(items, 32)
	if len(got) != 4 { // 32+32+32+4
		t.Fatalf("got %d batches, want 4", len(got))
	}
	for _, batch := range got {
		if len(batch) > 32 {
			t.Fatalf("batch size %d exceeds cap 32", len(batch))
		}
	}
	var flattened []string
	for _, batch := range got {
		flattened = append(flattened, batch...)
	}
	if len(flattened) != len(items) {
		t.Fatalf("flattened length = %d, want %d (no dropped/duplicated items)", len(flattened), len(items))
	}
	for i := range items {
		if flattened[i] != items[i] {
			t.Fatalf("order not preserved at index %d: got %q, want %q", i, flattened[i], items[i])
		}
	}
}

func TestBatchStringsEdgeCases(t *testing.T) {
	if got := batchStrings(nil, 10); got != nil {
		t.Fatalf("batchStrings(nil, 10) = %v, want nil", got)
	}
	if got := batchStrings([]string{"a"}, 0); got != nil {
		t.Fatalf("batchStrings with size=0 = %v, want nil", got)
	}
	if got := batchStrings([]string{"a", "b"}, 10); len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("batch size larger than input = %v, want a single batch of 2", got)
	}
}

// --- 006-pdf-layout-chunking US1: cross-page paragraph merging ---

// crossPageFixture is fixture F1 from quickstart.md §2.2: five pages whose
// page-3 last line is a mid-sentence break (no sentence-ending
// punctuation, not a list item or heading, close to the page's body line
// width) continued by page 4's first line. Pages 1, 2 and 5 end cleanly on
// punctuation so they must NOT be merged into their neighbours.
//
// The two halves carry marker tokens so an assertion can tell "both halves
// landed in one chunk" apart from "both halves exist somewhere".
const (
	crossPageTailMarker = "retentionwindowmarker"
	crossPageHeadMarker = "quarterlyreviewmarker"
)

func crossPageFixture(t testing.TB) string {
	t.Helper()
	body := func(n int) testLine {
		// Uniform-ish width so the page's body line-width mode is well
		// defined and the trailing line reads as "full width".
		return testLine{Text: fmt.Sprintf("body line %d of this page carries ordinary prose of a similar width.", n)}
	}
	return writeTestPDF(t, [][]testLine{
		{body(1), body(2), {Text: "page one ends cleanly on a full stop."}},
		{body(3), body(4), {Text: "page two also ends cleanly on a full stop."}},
		{
			body(5), body(6),
			// No terminal punctuation, full width: the merge candidate.
			{Text: "the " + crossPageTailMarker + " therefore applies to every archived record created before the"},
		},
		{
			{Text: "cutoff date and the " + crossPageHeadMarker + " keeps it under review."},
			body(7),
		},
		{body(8), {Text: "page five ends cleanly on a full stop."}},
	})
}

// TestChunkPDFCrossPageParagraphStaysWhole is SC-001's acceptance case.
//
// ⚠️ This MUST fail before the feature is implemented — chunkPDFPages
// chunks every page independently, so the two halves of the page-3/page-4
// paragraph land in two different chunks, each semantically incomplete.
// A version of this test that passes against the old implementation is
// not testing what it claims to and must be rewritten, not celebrated.
//
// The PageEnd half of the contract (this chunk must report pages 3-4, not
// pick one end) is asserted once chunkPiece carries PageEnd — see T021.
func TestChunkPDFCrossPageParagraphStaysWhole(t *testing.T) {
	parsed, err := parseFile(crossPageFixture(t), FileTypePDF)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	pieces := chunkDocument(FileTypePDF, parsed, 400, 40)
	if len(pieces) == 0 {
		t.Fatalf("expected chunks from a 5-page pdf, got none")
	}

	for _, p := range pieces {
		if !strings.Contains(p.Content, crossPageTailMarker) || !strings.Contains(p.Content, crossPageHeadMarker) {
			continue
		}
		if p.PageNumber == nil {
			t.Fatalf("cross-page chunk has no page number: %q", p.Content)
		}
		if *p.PageNumber != 3 {
			t.Fatalf("cross-page chunk starts at page %d, want 3 (the page the paragraph starts on)", *p.PageNumber)
		}
		return
	}

	t.Fatalf("no single chunk contains both halves of the cross-page paragraph;"+
		" it was cut at the page boundary. chunks:\n%s", chunkContentsForDiag(pieces))
}

func chunkContentsForDiag(pieces []chunkPiece) string {
	var sb strings.Builder
	for i, p := range pieces {
		page := "nil"
		if p.PageNumber != nil {
			page = fmt.Sprintf("%d", *p.PageNumber)
		}
		fmt.Fprintf(&sb, "  [%d] page=%s %q\n", i, page, p.Content)
	}
	return sb.String()
}

// BenchmarkPDFExtractAndChunk is SC-008's measurement, and its scope is
// deliberately narrow: extraction + chunking only, no embedding call and
// no database write.
//
// Measuring end-to-end ingestion instead would make the "≤ 50% slower"
// bar meaningless — the bulk of ingestion time is the embedding provider
// round-trip, which this feature does not touch, so any regression in
// chunking would disappear into the noise. Extraction + chunking is the
// only part this feature changes, so it is the only honest yardstick.
//
// The fixture is deliberately many pages with repeated headers/footers:
// noise detection needs cross-page statistics, and this is where an
// accidental O(pages²) would show up as a superlinear curve rather than a
// constant slowdown.
func BenchmarkPDFExtractAndChunk(b *testing.B) {
	const pageCount = 40
	pages := make([][]testLine, 0, pageCount)
	for i := 1; i <= pageCount; i++ {
		pages = append(pages, []testLine{
			{Text: "Hify Engineering Handbook", FontSize: 9},
			{Text: fmt.Sprintf("section %d covers the operational rules that apply to this area.", i)},
			{Text: fmt.Sprintf("each rule below is numbered so that section %d can be cited exactly.", i)},
			{Text: "the paragraph continues past the bottom of this page and picks up on the"},
			{Text: fmt.Sprintf("Page %d of %d", i, pageCount), FontSize: 9},
		})
	}
	path := writeTestPDF(b, pages)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parsed, err := parseFile(path, FileTypePDF)
		if err != nil {
			b.Fatalf("parseFile: %v", err)
		}
		if pieces := chunkDocument(FileTypePDF, parsed, 500, 50); len(pieces) == 0 {
			b.Fatalf("no chunks produced")
		}
	}
}

// --- 006-pdf-layout-chunking US2: running headers/footers (fixture F2) ---

const (
	f2Header = "Hify Engineering Handbook"
	f2Footer = "Confidential Internal Draft"
)

// headerFooterFixture is fixture F2: six pages (comfortably above
// minPagesForRepeatNoise), each with the same short header at the top and
// the same short footer plus a page-number line at the bottom, and body
// text that differs on every page.
func headerFooterFixture(t testing.TB) string {
	t.Helper()
	const pageCount = 6
	pages := make([][]testLine, 0, pageCount)
	for i := 1; i <= pageCount; i++ {
		// ⚠️ 正文行数刻意给足（8 行）。位置判据的带宽是**该页文字纵向跨度**的
		// 一个比例，页上只有三五行时整页跨度本身就很小，页脚会落在带外——那不
		// 是判据写错了，是夹具不像真实页面。真实文档一页几十行，15% 覆盖到的
		// 正是页眉页脚所在的那几行。
		lines := []testLine{{Text: f2Header, FontSize: 9}}
		for b := 1; b <= 8; b++ {
			lines = append(lines, testLine{
				Text: fmt.Sprintf("section %d body line %d of ordinary prose running the full column width.", i, b),
			})
		}
		lines = append(lines,
			testLine{Text: f2Footer, FontSize: 9},
			testLine{Text: fmt.Sprintf("Page %d of %d", i, pageCount), FontSize: 9},
		)
		pages = append(pages, lines)
	}
	return writeTestPDF(t, pages)
}

// TestChunkPDFHeaderFooterNeverReachChunks is SC-002: the count of header
// and footer occurrences across all produced chunks must be exactly 0.
//
// "Zero", not "few". Every occurrence that survives does two kinds of
// damage: it dilutes that chunk's embedding with text that says nothing
// about the chunk's subject, and — because the same words appear in chunks
// from unrelated pages — it makes those chunks spuriously similar to each
// other, so a query matching the header can drag back pages that have
// nothing to do with it.
func TestChunkPDFHeaderFooterNeverReachChunks(t *testing.T) {
	parsed, err := parseFile(headerFooterFixture(t), FileTypePDF)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	cleaned, records := stripLayoutNoise(parsed.Pages)
	parsed.Pages = cleaned

	pieces := chunkDocument(FileTypePDF, parsed, 400, 40)
	if len(pieces) == 0 {
		t.Fatal("expected chunks from a 6-page pdf")
	}

	headerHits, footerHits, pageNumHits := 0, 0, 0
	for _, p := range pieces {
		headerHits += strings.Count(p.Content, f2Header)
		footerHits += strings.Count(p.Content, f2Footer)
		for i := 1; i <= 6; i++ {
			pageNumHits += strings.Count(p.Content, fmt.Sprintf("Page %d of 6", i))
		}
	}
	if headerHits != 0 || footerHits != 0 || pageNumHits != 0 {
		t.Fatalf("页眉/页脚/页码行残留：header=%d footer=%d pageNumber=%d，SC-002 要求全部为 0。chunks:\n%s",
			headerHits, footerHits, pageNumHits, chunkContentsForDiag(pieces))
	}

	// FR-008: 剥离必须留下可事后核查的记录，而不是静悄悄地删。
	if len(records) == 0 {
		t.Fatal("剥离了内容却没有产生任何审计记录（FR-008）")
	}
	seen := map[noiseReason]bool{}
	for _, r := range records {
		seen[r.Reason] = true
		if r.Page < 1 || r.Page > 6 {
			t.Fatalf("审计记录的页码 %d 不在 1..6 内：%+v", r.Page, r)
		}
	}
	for _, want := range []noiseReason{reasonRepeatedHeader, reasonRepeatedFooter, reasonPageNumberLine} {
		if !seen[want] {
			t.Fatalf("审计记录里没有出现 %q 这一类原因：%+v", want, records)
		}
	}

	// 正文一行都不能少。
	for i := 1; i <= 6; i++ {
		want := fmt.Sprintf("section %d body line 1", i)
		found := false
		for _, p := range pieces {
			if strings.Contains(p.Content, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("正文 %q 在剥离过程中丢失了（SC-005 要求正文误删率为 0）", want)
		}
	}
}
