package knowledge

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// Unit tests for layout.go's heuristics (006-pdf-layout-chunking).
//
// Everything here runs on hand-written []pdfLine literals — no database,
// no PDF bytes, no parser. That is the whole point: these judgements are
// where the feature can silently do damage (merging two unrelated topics,
// or deleting a line of body text), and hand-written input is the only
// form in which every branch can be exercised exhaustively and cheaply.

// bodyLine builds an ordinary body line: default size, full column width.
func bodyLine(page int, y float64, text string) pdfLine {
	return pdfLine{Text: text, Page: page, FontSize: 12, Y: y, Width: 400}
}

// --- shouldMergeAcrossPage: the three criteria, and each failing alone ---

func TestShouldMergeAcrossPageAllThreeCriteriaHold(t *testing.T) {
	page := []pdfLine{
		bodyLine(3, 700, "the archived record remains subject to the retention"),
		bodyLine(3, 686, "window described above and is reviewed each quarter by the"),
	}
	if !shouldMergeAcrossPage(page[len(page)-1], page) {
		t.Fatalf("all three criteria hold (no terminator, not structural, full width) but merge was refused")
	}
}

// TestShouldMergeAcrossPageEachCriterionAloneBlocksIt is the FR-007-shaped
// half of the merge test: the criteria are an AND, so knocking out any one
// of them must block the merge on its own. A version of this that only
// checked the all-pass and all-fail cases would still pass if the AND had
// been written as an OR.
func TestShouldMergeAcrossPageEachCriterionAloneBlocksIt(t *testing.T) {
	cases := []struct {
		name string
		last pdfLine
		why  string
	}{
		{
			name: "criterion 1 fails: the line ends a sentence",
			last: bodyLine(3, 686, "window described above and is reviewed each quarter."),
			why:  "a finished sentence is a finished paragraph — nothing is dangling",
		},
		{
			name: "criterion 1 fails: full-width Chinese full stop",
			last: bodyLine(3, 686, "上述条款自公告之日起施行，不再另行通知。"),
			why:  "全角句号同样是句末标点",
		},
		{
			name: "criterion 1 fails: terminator behind a closing quote",
			last: bodyLine(3, 686, "所谓「留存期」的定义见前条。」"),
			why:  "收尾的引号后面仍然是一个已经结束的句子",
		},
		{
			name: "criterion 2 fails: numbered list item",
			last: pdfLine{Text: "3. 申请人应当提交下列材料", Page: 3, Y: 686, FontSize: 12, Width: 400},
			why:  "a list item is a structural boundary even with no punctuation",
		},
		{
			name: "criterion 2 fails: bulleted item",
			last: pdfLine{Text: "- 附具身份证明文件", Page: 3, Y: 686, FontSize: 12, Width: 400},
			why:  "同上，项目符号也是结构边界",
		},
		{
			name: "criterion 2 fails: chapter heading",
			last: pdfLine{Text: "第三章 档案的留存与销毁", Page: 3, Y: 686, FontSize: 12, Width: 400},
			why:  "中文标题通常不带句号，criterion 1 完全挡不住它",
		},
		{
			name: "criterion 2 fails: font size says heading",
			last: pdfLine{Text: "档案的留存与销毁", Page: 3, Y: 686, FontSize: 20, Width: 400},
			why:  "字号显著大于正文众数，是标题而不是被截断的正文",
		},
		{
			name: "criterion 3 fails: line stops well short of the column",
			last: pdfLine{Text: "window described above", Page: 3, Y: 686, FontSize: 12, Width: 120},
			why:  "a short line ended because the paragraph did, not because the page did",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := []pdfLine{
				bodyLine(3, 714, "the archived record remains subject to the retention"),
				bodyLine(3, 700, "policy set out in the preceding section of this handbook"),
				tc.last,
			}
			if shouldMergeAcrossPage(tc.last, page) {
				t.Fatalf("merge was allowed but should not be — %s", tc.why)
			}
		})
	}
}

func TestHasHeadingFontSizeUnknownSizeIsNeverASignal(t *testing.T) {
	page := []pdfLine{
		bodyLine(1, 700, "ordinary body text on this page"),
		bodyLine(1, 686, "more ordinary body text on this page"),
		{Text: "size unknown", Page: 1, Y: 672, FontSize: 0, Width: 400},
	}
	// FontSize 0 means "the extractor could not tell", never "0pt". It must
	// not be compared numerically in either direction: not as a heading
	// (it isn't known to be bigger) and not as evidence of body text
	// (it isn't known to be the same either).
	if hasHeadingFontSize(page[2], page) {
		t.Fatalf("unknown font size (0) was treated as a heading signal — FR-016 forbids inferring from a missing signal")
	}
}

// --- buildParagraphStream: page intervals and where merging may happen ---

// streamPage builds a pdfPage with real line geometry, so the stream code
// takes its normal path rather than the no-geometry fallback.
func streamPage(number int, texts ...string) pdfPage {
	lines := make([]pdfLine, 0, len(texts))
	y := 700.0
	for _, txt := range texts {
		lines = append(lines, pdfLine{Text: txt, Page: number, FontSize: 12, Y: y, Width: 400})
		y -= 14
	}
	joined := make([]string, len(lines))
	for i, l := range lines {
		joined[i] = l.Text
	}
	return pdfPage{Number: number, Text: strings.Join(joined, "\n"), Lines: lines}
}

func TestBuildParagraphStreamMergesOnlyTheDanglingSeam(t *testing.T) {
	pages := []pdfPage{
		streamPage(1, "page one closes on a full stop."),
		streamPage(2, "page two ends mid-clause and carries on to the next page which"),
		streamPage(3, "continues here and then finishes properly."),
	}
	stream := buildParagraphStream(pages)

	if len(stream) != 2 {
		t.Fatalf("got %d units, want 2 (page 1 alone; pages 2-3 joined): %+v", len(stream), stream)
	}
	if stream[0].PageStart != 1 || stream[0].PageEnd != 1 {
		t.Fatalf("unit 0 = pages %d-%d, want 1-1", stream[0].PageStart, stream[0].PageEnd)
	}
	if stream[1].PageStart != 2 || stream[1].PageEnd != 3 {
		t.Fatalf("unit 1 = pages %d-%d, want 2-3", stream[1].PageStart, stream[1].PageEnd)
	}
	if !strings.Contains(stream[1].Content, "carries on") || !strings.Contains(stream[1].Content, "continues here") {
		t.Fatalf("merged unit lost one of its halves: %q", stream[1].Content)
	}
}

// TestBuildParagraphStreamNeverJoinsNonAdjacentPages covers the case a
// scanned or blank page sits between two text pages. The text either side
// is genuinely discontinuous; joining it would fabricate both the
// continuity and the page interval that describes it.
func TestBuildParagraphStreamNeverJoinsNonAdjacentPages(t *testing.T) {
	pages := []pdfPage{
		streamPage(1, "this page ends mid-clause and would love to continue onto the"),
		// page 2 has no extractable text at all — a scanned page
		streamPage(3, "an entirely unrelated passage begins here."),
	}
	stream := buildParagraphStream(pages)
	if len(stream) != 2 {
		t.Fatalf("got %d units, want 2 — pages 1 and 3 are not adjacent and must not merge: %+v", len(stream), stream)
	}
	for _, u := range stream {
		if u.PageStart != u.PageEnd {
			t.Fatalf("unit spans pages %d-%d across a gap that has no text: %+v", u.PageStart, u.PageEnd, u)
		}
	}
}

// TestBuildParagraphStreamIntervalsSatisfyInvariants pins P1/P2/P3 and
// C3's upper bound, which no database constraint can check.
func TestBuildParagraphStreamIntervalsSatisfyInvariants(t *testing.T) {
	pages := []pdfPage{
		streamPage(1, "first page runs on without stopping and keeps going into the"),
		streamPage(2, "second page where it finally ends. a second sentence follows."),
		streamPage(3, "third page stands alone and ends cleanly."),
	}
	stream := buildParagraphStream(pages)
	if len(stream) == 0 {
		t.Fatal("expected units")
	}
	prevStart := 0
	for i, u := range stream {
		if strings.TrimSpace(u.Content) == "" {
			t.Fatalf("unit %d is blank (P1)", i)
		}
		if u.PageStart < 1 || u.PageStart > u.PageEnd || u.PageEnd > len(pages) {
			t.Fatalf("unit %d has interval %d-%d, outside 1..%d (P2)", i, u.PageStart, u.PageEnd, len(pages))
		}
		if u.PageStart < prevStart {
			t.Fatalf("unit %d starts on page %d, before unit %d's page %d (P4)", i, u.PageStart, i-1, prevStart)
		}
		prevStart = u.PageStart
	}
}

// --- SC-004: determinism, specifically the map-iteration trap ---

// TestModalStatisticsAreDeterministicOnTies is the one test standing
// between this feature and a bug that shows up once in dozens of runs.
//
// Both modal statistics count into a map. Ranging over a map to find the
// maximum picks an arbitrary winner among tied buckets, so the same PDF
// would chunk differently on different runs — and the input where that
// actually bites is precisely a TIE, which is why both fixtures here are
// built to tie rather than to have a clear winner.
//
// ⚠️ If this test is ever seen to pass while the implementation ranges a
// map directly, the repeat count is too low, not the test wrong. Raise it.
func TestModalStatisticsAreDeterministicOnTies(t *testing.T) {
	const repeats = 200

	// Two width buckets with exactly two lines each: a tie.
	tiedWidths := []pdfLine{
		{Text: "a", Page: 1, Y: 700, FontSize: 12, Width: 200},
		{Text: "b", Page: 1, Y: 686, FontSize: 12, Width: 200},
		{Text: "c", Page: 1, Y: 672, FontSize: 12, Width: 400},
		{Text: "d", Page: 1, Y: 658, FontSize: 12, Width: 400},
	}
	wantWidth := modalLineWidth(tiedWidths)
	for i := 0; i < repeats; i++ {
		if got := modalLineWidth(tiedWidths); got != wantWidth {
			t.Fatalf("modalLineWidth run %d = %v, first run = %v — the mode depends on map iteration order", i, got, wantWidth)
		}
	}
	// Ties resolve to the wider bucket: between two equally common widths,
	// the wider one is the better stand-in for "full column width".
	if wantWidth != 400 {
		t.Fatalf("modalLineWidth tie resolved to %v, want 400 (ties go to the wider bucket)", wantWidth)
	}

	// Two font sizes with exactly two lines each: a tie.
	tiedSizes := []pdfLine{
		{Text: "a", Page: 1, Y: 700, FontSize: 12, Width: 400},
		{Text: "b", Page: 1, Y: 686, FontSize: 12, Width: 400},
		{Text: "c", Page: 1, Y: 672, FontSize: 18, Width: 400},
		{Text: "d", Page: 1, Y: 658, FontSize: 18, Width: 400},
	}
	wantSize := modalBodyFontSize(tiedSizes)
	for i := 0; i < repeats; i++ {
		if got := modalBodyFontSize(tiedSizes); got != wantSize {
			t.Fatalf("modalBodyFontSize run %d = %v, first run = %v — the mode depends on map iteration order", i, got, wantSize)
		}
	}
	// Ties resolve to the SMALLER size here: the body is the smaller text,
	// and resolving upwards would raise the bar a heading has to clear.
	if wantSize != 12 {
		t.Fatalf("modalBodyFontSize tie resolved to %v, want 12 (ties go to the smaller size)", wantSize)
	}
}

// TestBuildParagraphStreamIsDeterministic runs the whole stream builder
// repeatedly on one input — the end-to-end form of the same guarantee.
func TestBuildParagraphStreamIsDeterministic(t *testing.T) {
	pages := []pdfPage{
		streamPage(1, "alpha runs to the margin and keeps going onto the following",
			"page where it eventually stops."),
		streamPage(2, "beta line one continues without stopping at all which means it",
			"beta line two finishes here."),
		streamPage(3, "gamma stands alone."),
	}
	first := fmt.Sprintf("%+v", buildParagraphStream(pages))
	for i := 0; i < 200; i++ {
		if got := fmt.Sprintf("%+v", buildParagraphStream(pages)); got != first {
			t.Fatalf("run %d differs from run 0:\n got: %s\nwant: %s", i, got, first)
		}
	}
}

// --- chunkPDFStream: intervals, length limits, txt/md untouched ---

func TestChunkPDFStreamRespectsSizeLimitOnMergedUnits(t *testing.T) {
	const size = 40
	units := []paragraphUnit{{
		Content:   strings.Repeat("mergedword ", 30),
		PageStart: 3,
		PageEnd:   4,
	}}
	pieces := chunkPDFStream(units, size, 5)
	if len(pieces) < 2 {
		t.Fatalf("a 330-rune unit at size %d must split, got %d pieces", size, len(pieces))
	}
	// FR-004: merging changes what counts as a paragraph, it does not
	// exempt anything from the size budget.
	assertChunksWithinSize(t, pieces, size)
	for i, p := range pieces {
		if p.PageNumber == nil || p.PageEnd == nil {
			t.Fatalf("piece %d lost its page interval: %+v", i, p)
		}
		if *p.PageNumber != 3 || *p.PageEnd != 4 {
			t.Fatalf("piece %d reports pages %d-%d, want 3-4", i, *p.PageNumber, *p.PageEnd)
		}
	}
}

func TestChunkPDFStreamKeepsSinglePageUnitsOnOnePage(t *testing.T) {
	units := []paragraphUnit{
		{Content: "第一页的一段正文。", PageStart: 1, PageEnd: 1},
		{Content: "第二页的一段正文。", PageStart: 2, PageEnd: 2},
	}
	pieces := chunkPDFStream(units, 200, 0)
	if len(pieces) != 2 {
		t.Fatalf("got %d pieces, want 2 — an unmerged page boundary is still a boundary: %+v", len(pieces), pieces)
	}
	for i, p := range pieces {
		if *p.PageNumber != i+1 || *p.PageEnd != i+1 {
			t.Fatalf("piece %d reports pages %d-%d, want %d-%d", i, *p.PageNumber, *p.PageEnd, i+1, i+1)
		}
	}
}

// TestChunkPDFStreamListTypeContentIsNotSweptIntoOneUnit is fixture F6:
// a list-shaped document with no sentence punctuation anywhere. The merge
// must not treat "no full stop" as "keep going forever" — criterion 2
// (list item) is what stops it, and the size limit is the backstop.
func TestChunkPDFStreamListTypeContentIsNotSweptIntoOneUnit(t *testing.T) {
	pages := make([]pdfPage, 0, 4)
	for p := 1; p <= 4; p++ {
		pages = append(pages, streamPage(p,
			fmt.Sprintf("%d. 第 %d 页的第一个列表项", p*10+1, p),
			fmt.Sprintf("%d. 第 %d 页的第二个列表项", p*10+2, p),
		))
	}
	stream := buildParagraphStream(pages)
	for _, u := range stream {
		if u.PageStart != u.PageEnd {
			t.Fatalf("list items were merged across pages %d-%d: %q", u.PageStart, u.PageEnd, u.Content)
		}
	}

	const size = 60
	pieces := chunkPDFStream(stream, size, 5)
	assertChunksWithinSize(t, pieces, size)
	if len(pieces) < 4 {
		t.Fatalf("4 pages of list items collapsed into %d pieces — the whole document was swept into one run", len(pieces))
	}
}

// TestChunkPDFStreamSinglePageDocumentIsUnchanged is the Edge Case "a
// one-page PDF must behave exactly as before": there is no seam, so there
// is nothing for this feature to do.
func TestChunkPDFStreamSinglePageDocumentIsUnchanged(t *testing.T) {
	page := streamPage(1,
		"第一段正文在这里结束。",
		"第二段正文也在这里结束。")
	pieces := chunkPDFPages([]pdfPage{page}, 200, 0)
	if len(pieces) == 0 {
		t.Fatal("expected pieces from a single-page document")
	}
	for i, p := range pieces {
		if p.PageNumber == nil || p.PageEnd == nil {
			t.Fatalf("piece %d lost its page interval: %+v", i, p)
		}
		if *p.PageNumber != 1 || *p.PageEnd != 1 {
			t.Fatalf("piece %d reports pages %d-%d on a one-page document, want 1-1", i, *p.PageNumber, *p.PageEnd)
		}
	}
}

// TestChunkDocumentNonPDFNeverCarriesPages pins invariant C4 / FR-014 /
// FR-020: txt and md have no notion of a page, and this feature must not
// have quietly given them one.
func TestChunkDocumentNonPDFNeverCarriesPages(t *testing.T) {
	cases := []struct {
		fileType string
		parsed   parsedContent
	}{
		{FileTypeTxt, parsedContent{Text: "第一段。\n\n第二段。"}},
		{FileTypeMD, parsedContent{Text: "# 标题\n\n正文一段。\n\n## 二级标题\n\n正文两段。"}},
	}
	for _, tc := range cases {
		t.Run(tc.fileType, func(t *testing.T) {
			pieces := chunkDocument(tc.fileType, tc.parsed, 100, 10)
			if len(pieces) == 0 {
				t.Fatal("expected pieces")
			}
			for i, p := range pieces {
				if p.PageNumber != nil || p.PageEnd != nil {
					t.Fatalf("piece %d of a %s document carries a page interval: %+v", i, tc.fileType, p)
				}
			}
		})
	}
}

// --- SC-005: noise stripping must never remove body text ---

// noisePage builds a page from explicit (text, width) pairs, laid out top
// to bottom, so a test can put a specific line in the margin band or in the
// body and give it a specific width.
type noiseTestLine struct {
	Text  string
	Width float64
}

func noisePage(number int, lines ...noiseTestLine) pdfPage {
	out := make([]pdfLine, 0, len(lines))
	y := 700.0
	for _, l := range lines {
		w := l.Width
		if w == 0 {
			w = 400
		}
		out = append(out, pdfLine{Text: l.Text, Page: number, FontSize: 12, Y: y, Width: w})
		y -= 14
	}
	texts := make([]string, len(out))
	for i, l := range out {
		texts[i] = l.Text
	}
	return pdfPage{Number: number, Text: strings.Join(texts, "\n"), Lines: out}
}

// body builds four full-width body lines, so a page has a well-defined
// modal width and a real middle region.
func bodyLines(page int) []noiseTestLine {
	return []noiseTestLine{
		{Text: fmt.Sprintf("第 %d 页的正文第一行，占满整个版心的宽度。", page)},
		{Text: fmt.Sprintf("第 %d 页的正文第二行，同样占满整个版心。", page)},
		{Text: fmt.Sprintf("第 %d 页的正文第三行，也是满行宽的。", page)},
		{Text: fmt.Sprintf("第 %d 页的正文第四行，收尾。", page)},
	}
}

func stripped(t *testing.T, pages []pdfPage) (map[string]bool, []noiseRecord) {
	t.Helper()
	cleaned, records := stripLayoutNoise(pages)
	remaining := map[string]bool{}
	for _, p := range cleaned {
		for _, l := range p.Lines {
			remaining[l.Text] = true
		}
	}
	return remaining, records
}

// TestStripLayoutNoiseEachCriterionAloneIsNotEnough is SC-005's real
// content and FR-007's direct test: the three criteria are an AND. Each of
// the first three cases satisfies two criteria and fails one, and each MUST
// survive. A version of this test that only checked "all three → stripped"
// and "none → kept" would still pass if the AND had been written as an OR,
// and an OR here deletes body text.
func TestStripLayoutNoiseEachCriterionAloneIsNotEnough(t *testing.T) {
	const pageCount = 6

	t.Run("在页顶但只出现在单页 -> 不剥离", func(t *testing.T) {
		// ⚠️ 各页的顶部小标题必须是**真正不同的词**，不能只差一个数字：
		// 归一化会抹掉数字（好让"第 3 页"和"第 4 页"算同一个页眉），
		// 只差数字的两行归一化后是同一个 key，那本来就该被判为页眉。
		titles := []string{"绪论", "背景与动机", "系统设计", "实现细节", "评估方法", "结论"}
		pages := make([]pdfPage, 0, pageCount)
		for i := 1; i <= pageCount; i++ {
			lines := []noiseTestLine{{Text: titles[i-1], Width: 100}}
			pages = append(pages, noisePage(i, append(lines, bodyLines(i)...)...))
		}
		remaining, records := stripped(t, pages)
		for _, title := range titles {
			if !remaining[title] {
				t.Fatalf("位置与长度都成立、但只出现在单页的内容 %q 被删掉了——重复率判据没有起作用", title)
			}
		}
		if len(records) != 0 {
			t.Fatalf("不该有任何剥离记录，实际 %d 条：%+v", len(records), records)
		}
	})

	t.Run("跨页高度重复但位于页面中部 -> 不剥离", func(t *testing.T) {
		pages := make([]pdfPage, 0, pageCount)
		for i := 1; i <= pageCount; i++ {
			lines := bodyLines(i)
			// 插在正中间：位置判据不成立。
			mid := append([]noiseTestLine{}, lines[:2]...)
			mid = append(mid, noiseTestLine{Text: "本条所称留存期", Width: 100})
			mid = append(mid, lines[2:]...)
			pages = append(pages, noisePage(i, mid...))
		}
		remaining, _ := stripped(t, pages)
		if !remaining["本条所称留存期"] {
			t.Fatal("页面中部的内容被删掉了——版心区域必须完全免疫，不管它重复多少次")
		}
	})

	t.Run("在页顶且跨页重复但接近正文行宽 -> 不剥离", func(t *testing.T) {
		pages := make([]pdfPage, 0, pageCount)
		for i := 1; i <= pageCount; i++ {
			lines := []noiseTestLine{{Text: "这一行每页都出现，但它和正文一样长，不像页眉", Width: 400}}
			pages = append(pages, noisePage(i, append(lines, bodyLines(i)...)...))
		}
		remaining, _ := stripped(t, pages)
		if !remaining["这一行每页都出现，但它和正文一样长，不像页眉"] {
			t.Fatal("位置与重复率都成立、但宽度接近正文的内容被删掉了——长度判据没有起作用")
		}
	})

	t.Run("三条同时成立 -> 剥离", func(t *testing.T) {
		pages := make([]pdfPage, 0, pageCount)
		for i := 1; i <= pageCount; i++ {
			lines := []noiseTestLine{{Text: "Hify 工程手册", Width: 100}}
			pages = append(pages, noisePage(i, append(lines, bodyLines(i)...)...))
		}
		remaining, records := stripped(t, pages)
		if remaining["Hify 工程手册"] {
			t.Fatal("三条判据同时成立的页眉没有被剥离")
		}
		if len(records) != pageCount {
			t.Fatalf("审计记录 %d 条，应当每页一条共 %d 条", len(records), pageCount)
		}
		if records[0].Reason != reasonRepeatedHeader {
			t.Fatalf("剥离原因是 %q，应当是 %q", records[0].Reason, reasonRepeatedHeader)
		}
	})
}

// TestStripLayoutNoisePageNumberLines covers the independent rule: a line
// that is nothing but a page marker never repeats verbatim, so no
// repetition threshold could catch it.
func TestStripLayoutNoisePageNumberLines(t *testing.T) {
	// "3" 在 5 页文档里是一个**可能的**页码，且被放在页面最外侧那一行；
	// 其余几种是无歧义的页码形态，不需要额外佐证。
	markers := []string{"3", "3 / 12", "第 4 页", "第 4 页 / 共 12 页", "Page 5 of 12"}
	for _, marker := range markers {
		t.Run(marker, func(t *testing.T) {
			pages := make([]pdfPage, 0, 5)
			for i := 1; i <= 5; i++ {
				lines := bodyLines(i)
				lines = append(lines, noiseTestLine{Text: marker, Width: 40})
				pages = append(pages, noisePage(i, lines...))
			}
			remaining, records := stripped(t, pages)
			if remaining[marker] {
				t.Fatalf("纯页码行 %q 没有被剥离", marker)
			}
			if len(records) == 0 || records[0].Reason != reasonPageNumberLine {
				t.Fatalf("剥离原因不是 %q：%+v", reasonPageNumberLine, records)
			}
		})
	}
}

// TestStripLayoutNoisePageNumberRuleStillRespectsThePositionBand pins the
// deliberate deviation documented on noiseVerdict: a bare number in the
// MIDDLE of a page is a table cell, not a page number, and deleting it
// would be exactly the silent body-text loss SC-005 rules out.
func TestStripLayoutNoisePageNumberRuleStillRespectsThePositionBand(t *testing.T) {
	pages := make([]pdfPage, 0, 5)
	for i := 1; i <= 5; i++ {
		lines := bodyLines(i)
		mid := append([]noiseTestLine{}, lines[:2]...)
		mid = append(mid, noiseTestLine{Text: "42", Width: 40})
		mid = append(mid, lines[2:]...)
		pages = append(pages, noisePage(i, mid...))
	}
	remaining, _ := stripped(t, pages)
	if !remaining["42"] {
		t.Fatal("页面中部一个只有数字的表格单元被当成页码删掉了")
	}
}

// TestStripLayoutNoiseBelowPageThresholdStripsNothing is FR-009 and
// fixture F7: on a two-page document any line appearing twice sits at 100%,
// which is not a statistic. Rather miss a header than delete body text.
func TestStripLayoutNoiseBelowPageThresholdStripsNothing(t *testing.T) {
	pages := []pdfPage{
		noisePage(1, append([]noiseTestLine{{Text: "Hify 工程手册", Width: 100}}, bodyLines(1)...)...),
		noisePage(2, append([]noiseTestLine{{Text: "Hify 工程手册", Width: 100}}, bodyLines(2)...)...),
	}
	remaining, records := stripped(t, pages)
	if !remaining["Hify 工程手册"] {
		t.Fatalf("两页文档的页眉被剥离了——页数低于门槛 %d 时必须什么都不删", minPagesForRepeatNoise)
	}
	if len(records) != 0 {
		t.Fatalf("两页文档产生了 %d 条剥离记录，应当是 0：%+v", len(records), records)
	}
}

// TestStripLayoutNoiseIsDeterministic — same map-iteration concern as the
// modal statistics, on the repetition counter this time.
func TestStripLayoutNoiseIsDeterministic(t *testing.T) {
	pages := make([]pdfPage, 0, 6)
	for i := 1; i <= 6; i++ {
		lines := []noiseTestLine{{Text: "Hify 工程手册", Width: 100}}
		lines = append(lines, bodyLines(i)...)
		// 两个并列的重复候选，逼出 tie-break。
		lines = append(lines, noiseTestLine{Text: "内部资料", Width: 100})
		lines = append(lines, noiseTestLine{Text: "第 " + strconv.Itoa(i) + " 页", Width: 40})
		pages = append(pages, noisePage(i, lines...))
	}
	cleaned, records := stripLayoutNoise(pages)
	want := fmt.Sprintf("%+v|%+v", cleaned, records)
	for i := 0; i < 200; i++ {
		c, r := stripLayoutNoise(pages)
		if got := fmt.Sprintf("%+v|%+v", c, r); got != want {
			t.Fatalf("第 %d 次剥离结果与第 0 次不同——判定依赖了 map 迭代顺序", i)
		}
	}
}

// TestNormalizeNoiseTextTreatsDigitOnlyDifferencesAsTheSameLine makes the
// normalisation's most consequential behaviour explicit rather than
// incidental: "第 3 页 / 共 12 页" and "第 4 页 / 共 12 页" have to count as
// the same footer, or a paginated footer would never reach the repetition
// threshold at all.
//
// The cost is real and worth stating: two genuinely different body lines
// that differ ONLY by a number ("表 1"/"表 2") also collapse together, and
// if they sit in the margin band and are short, they can be stripped. In
// practice a line that appears at the top or bottom of most pages and
// differs only by a counter IS page furniture — but this is the sharpest
// edge of the noise rule, and it is documented here rather than discovered.
func TestNormalizeNoiseTextTreatsDigitOnlyDifferencesAsTheSameLine(t *testing.T) {
	same := []string{"第 3 页 / 共 12 页", "第 4 页 / 共 12 页", "第 11 页 / 共 12 页"}
	want := normalizeNoiseText(same[0])
	for _, s := range same[1:] {
		if got := normalizeNoiseText(s); got != want {
			t.Fatalf("normalizeNoiseText(%q) = %q, want %q — 分页页脚必须归一化成同一个 key", s, got, want)
		}
	}
	if got := normalizeNoiseText("完全不同的一行"); got == want {
		t.Fatalf("内容不同的两行被归一化成了同一个 key %q", got)
	}
}

// --- US4 (P3)：标题识别与拼进正文 ---

// TestHeadingLevelRequiresBothSignals is the cross-validation contract: a
// line must be BOTH visibly larger than the body AND heading-shaped. Either
// signal alone is a bad detector, and FR-016 says an unrecognisable heading
// stays blank rather than being invented.
func TestHeadingLevelRequiresBothSignals(t *testing.T) {
	body := []pdfLine{
		bodyLine(1, 700, "ordinary body text at the usual size"),
		bodyLine(1, 686, "more ordinary body text at the usual size"),
		bodyLine(1, 672, "still more ordinary body text at the usual size"),
	}
	cases := []struct {
		name string
		line pdfLine
		want int
	}{
		{"字号大 + 编号模式 -> 一级标题", pdfLine{Text: "1. 系统设计", FontSize: 20, Width: 200}, 1},
		{"字号大 + 二级编号 -> 二级标题", pdfLine{Text: "1.2 检索链路", FontSize: 20, Width: 200}, 2},
		{"字号大 + 章模式 -> 一级标题", pdfLine{Text: "第三章 档案留存", FontSize: 20, Width: 200}, 1},
		{"字号大 + 短全大写 -> 一级标题", pdfLine{Text: "SYSTEM DESIGN", FontSize: 20, Width: 200}, 1},
		{"只有字号大（普通句子）-> 不是标题", pdfLine{Text: "这句话只是被放大了而已", FontSize: 20, Width: 200}, 0},
		{"只有编号模式（字号是正文）-> 不是标题", pdfLine{Text: "1. 这其实是个列表项", FontSize: 12, Width: 200}, 0},
		{"字号未知 -> 不是标题", pdfLine{Text: "1. 系统设计", FontSize: 0, Width: 200}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := append(append([]pdfLine{}, body...), tc.line)
			if got := headingLevel(tc.line, lines); got != tc.want {
				t.Fatalf("headingLevel = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestChunkPDFStreamHeadingPathIsInsideTheChunkText is FR-015's actual
// requirement, and the reason the wording is emphatic: writing the section
// title only into a metadata field satisfies nothing, because only the
// chunk TEXT is embedded. A chunk saying "它支持三种模式" is unretrievable
// unless the heading it sits under travels with it.
func TestChunkPDFStreamHeadingPathIsInsideTheChunkText(t *testing.T) {
	page := pdfPage{Number: 1, Lines: []pdfLine{
		{Text: "1. 检索链路", Page: 1, FontSize: 20, Y: 700, Width: 150},
		{Text: "这一段正文本身没有提到检索链路四个字。", Page: 1, FontSize: 12, Y: 680, Width: 400},
	}}
	page.Text = page.Lines[0].Text + "\n" + page.Lines[1].Text

	pieces := chunkPDFPages([]pdfPage{page}, 200, 0)
	if len(pieces) == 0 {
		t.Fatal("expected pieces")
	}
	found := false
	for _, p := range pieces {
		if !strings.Contains(p.Content, "这一段正文本身") {
			continue
		}
		found = true
		if !strings.Contains(p.Content, "检索链路") {
			t.Fatalf("标题路径没有拼进片段正文，只放进元数据不满足 FR-015：%q", p.Content)
		}
		if p.SectionTitle == nil || *p.SectionTitle != "1. 检索链路" {
			t.Fatalf("SectionTitle = %v，应当是 %q", p.SectionTitle, "1. 检索链路")
		}
	}
	if !found {
		t.Fatalf("正文片段不见了：%+v", pieces)
	}
}

// TestChunkPDFStreamHeadingNeverStarvesTheBody is FR-015a: when the
// breadcrumb and the body compete for the size budget, the BODY wins and
// the breadcrumb shortens or disappears — the same precedence the Markdown
// path already had, reused rather than reimplemented.
func TestChunkPDFStreamHeadingNeverStarvesTheBody(t *testing.T) {
	const size = 40
	body := "正文必须完整保留，一个字都不能被标题挤掉。"
	units := []paragraphUnit{{
		Content:   body,
		PageStart: 1,
		PageEnd:   1,
		Headings:  []string{strings.Repeat("很长的标题", 20)},
	}}
	pieces := chunkPDFStream(units, size, 0)
	assertChunksWithinSize(t, pieces, size)
	joined := ""
	for _, p := range pieces {
		joined += p.Content
	}
	if !strings.Contains(joined, body) {
		t.Fatalf("正文被标题挤掉了（FR-015a 要求优先保全正文）：%q", joined)
	}
}

// TestChunkPDFStreamNoRecognisableHeadingLeavesItBlank — FR-016.
func TestChunkPDFStreamNoRecognisableHeadingLeavesItBlank(t *testing.T) {
	page := streamPage(1, "全篇没有任何可辨识的标题结构。", "只是两段普通正文而已。")
	for _, p := range chunkPDFPages([]pdfPage{page}, 200, 0) {
		if p.SectionTitle != nil {
			t.Fatalf("没有可辨识的标题结构却编造了一个 %q", *p.SectionTitle)
		}
	}
}

// TestStripLayoutNoiseBareNumberNeedsCorroboration 是跑真实论文逼出来的用例。
//
// ⚠️ 发现经过：把一篇 15 页的 arXiv 论文喂进流水线，**20 行被当成纯页码行
// 删掉了**——学术 PDF 里满是"只有一个数字"的行：抽取器单独成行的上下标、
// 脚注标记、公式编号。它们是正文。SC-005 要求正文误删率为 0，所以"只有数字"
// 这一种形态必须额外佐证：既要是该页最外侧的那一行，数值也要落在文档真实的
// 页数范围内。无歧义的那几种形态（"第 3 页"/"Page 3 of 12"）不受此限。
func TestStripLayoutNoiseBareNumberNeedsCorroboration(t *testing.T) {
	build := func(inner noiseTestLine, atEdge bool) []pdfPage {
		pages := make([]pdfPage, 0, 5)
		for i := 1; i <= 5; i++ {
			lines := bodyLines(i)
			if atEdge {
				lines = append(lines, inner)
			} else {
				mid := append([]noiseTestLine{}, lines[:2]...)
				mid = append(mid, inner)
				lines = append(mid, lines[2:]...)
			}
			pages = append(pages, noisePage(i, lines...))
		}
		return pages
	}

	t.Run("数值超出文档页数 -> 保留（不可能是页码）", func(t *testing.T) {
		remaining, _ := stripped(t, build(noiseTestLine{Text: "97", Width: 30}, true))
		if !remaining["97"] {
			t.Fatal("5 页文档里的 \"97\" 被当成页码删了——它不可能是这份文档的页码")
		}
	})

	t.Run("不在页面最外侧 -> 保留（更像上下标/脚注标记）", func(t *testing.T) {
		remaining, _ := stripped(t, build(noiseTestLine{Text: "2", Width: 30}, false))
		if !remaining["2"] {
			t.Fatal("夹在正文行之间的 \"2\" 被当成页码删了——这正是论文里上下标的位置")
		}
	})

	t.Run("最外侧 + 数值合理 -> 剥离", func(t *testing.T) {
		remaining, records := stripped(t, build(noiseTestLine{Text: "2", Width: 30}, true))
		if remaining["2"] {
			t.Fatal("页面最外侧、数值合理的纯数字行没有被剥离")
		}
		if len(records) == 0 || records[0].Reason != reasonPageNumberLine {
			t.Fatalf("剥离原因不是 %q：%+v", reasonPageNumberLine, records)
		}
	})
}

// TestBuildParagraphStreamCapsMergeSpan 是 006 §7.3 的修复。
//
// ⚠️ 发现经过：把一份**全篇没有句末标点**的 10 页 PDF 喂进流水线，每个页边界的
// 三条合并判据都成立（没有句号、不是列表、满行宽），于是整篇合并成 **1 个单元**，
// 34 个片段**全部标成「第 1-10 页」**。长度上限确实生效了（FR-004 没破），
// 但页码区间退化成「本文档某处」，引用价值归零。
//
// spec 的 Edge Case 只要求「不得合并成一个超长段落」，没要求「区间不得退化」
// ——这条边界当时画得不够。
func TestBuildParagraphStreamCapsMergeSpan(t *testing.T) {
	// 10 页，每页一行，全篇无句末标点、行宽一致——三条判据在每个页边界都成立。
	pages := make([]pdfPage, 0, 10)
	for i := 1; i <= 10; i++ {
		pages = append(pages, streamPage(i,
			fmt.Sprintf("page %d text that runs to the margin and never ends a sentence", i)))
	}
	stream := buildParagraphStream(pages)

	if len(stream) < 2 {
		t.Fatalf("10 页无标点文档合并成了 %d 个单元——整篇被吞成一个，页码区间退化成「本文档某处」", len(stream))
	}
	for i, u := range stream {
		span := u.PageEnd - u.PageStart + 1
		if span > maxMergedPageSpan {
			t.Fatalf("单元 %d 跨了 %d 页（%d-%d），上限是 %d", i, span, u.PageStart, u.PageEnd, maxMergedPageSpan)
		}
	}
	// 覆盖必须完整：加了上限不等于可以丢内容。
	seen := map[int]bool{}
	for _, u := range stream {
		for p := u.PageStart; p <= u.PageEnd; p++ {
			seen[p] = true
		}
	}
	for p := 1; p <= 10; p++ {
		if !seen[p] {
			t.Fatalf("第 %d 页的内容在加了合并上限之后丢失了", p)
		}
	}
}

// TestBuildParagraphStreamCapDoesNotAffectRealParagraphs：上限不得影响正常
// 文档——真实散文里一个段落不会跨三页，所以这条上限对它们完全不可见。
func TestBuildParagraphStreamCapDoesNotAffectRealParagraphs(t *testing.T) {
	pages := []pdfPage{
		streamPage(1, "the retention policy applies to every archived record created"),
		streamPage(2, "before the cutoff date and is reviewed each quarter."),
		streamPage(3, "an unrelated section starts here and ends cleanly."),
	}
	stream := buildParagraphStream(pages)
	if len(stream) != 2 {
		t.Fatalf("got %d units, want 2（页 1-2 合并、页 3 独立）：%+v", len(stream), stream)
	}
	if stream[0].PageStart != 1 || stream[0].PageEnd != 2 {
		t.Fatalf("跨页单元 = %d-%d，want 1-2——上限不该影响正常的两页合并",
			stream[0].PageStart, stream[0].PageEnd)
	}
}
