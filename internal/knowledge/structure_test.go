package knowledge

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Unit tests for phase-2 structure-aware chunking (chunk.go) and PDF
// per-page parsing (parse.go). Characterization-style, like chunk_test.go:
// each test pins down one contract the plan calls out — code/table blocks
// surviving intact, overlap never crossing a markdown section boundary,
// PDF chunks never spanning two pages, and (critically) plain fallback
// behavior for structureless content staying byte-for-byte identical to
// the old flat chunkText, since several DB integration tests already pin
// exact chunk counts for `strings.Repeat("a", N)` fixtures.

// assertChunksWithinSize is the shared invariant check for phase-2
// regression tests added to fix the overlap/breadcrumb budget bugs: every
// non-empty chunkPiece.Content must be <= size runes, full stop — a chunk
// count assertion alone (what several tests above this checked) cannot
// catch a chunk that's individually oversized while the count still looks
// right.
func assertChunksWithinSize(t *testing.T, pieces []chunkPiece, size int) {
	t.Helper()
	for i, p := range pieces {
		if got := len([]rune(p.Content)); got > size {
			t.Fatalf("piece %d exceeds size: got %d runes, want <= %d: %q", i, got, size, p.Content)
		}
	}
}

// assertStringsWithinSize is assertChunksWithinSize's counterpart for the
// plain-string-returning chunkers (chunkPlainText/chunkBySentence/
// chunkText), which have no breadcrumb but are subject to the same
// pendingOverlap-budget bug.
func assertStringsWithinSize(t *testing.T, chunks []string, size int) {
	t.Helper()
	for i, c := range chunks {
		if got := len([]rune(c)); got > size {
			t.Fatalf("chunk %d exceeds size: got %d runes, want <= %d: %q", i, got, size, c)
		}
	}
}

// --- chunkPlainText: paragraph/sentence aware TXT chunking ---

// TestChunkPlainTextOverlapNeverExceedsSizeAndKeepsBothParagraphs is the
// direct regression test for the reported bug: after flush(true), the
// carried-forward pendingOverlap used to be spliced onto the next
// paragraph with no budget check at all — overlap(4) + separator(1) +
// a fresh 8-rune paragraph totals 13 runes against a configured
// chunk_size of 10, silently exceeding it. Both paragraphs' full body text
// must still be present somewhere in the output; only the overlap portion
// is allowed to shrink.
func TestChunkPlainTextOverlapNeverExceedsSizeAndKeepsBothParagraphs(t *testing.T) {
	const size, overlap = 10, 4
	para1 := "AAAAAAAA" // 8 runes
	para2 := "BBBBBBBB" // 8 runes
	got := chunkPlainText(para1+"\n\n"+para2, size, overlap)

	assertStringsWithinSize(t, got, size)

	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, para1) {
		t.Fatalf("first paragraph lost, got chunks: %v", got)
	}
	if !strings.Contains(joined, para2) {
		t.Fatalf("second paragraph lost, got chunks: %v", got)
	}
}

// TestChunkPlainTextSentenceSplitOverlapStaysWithinSizeAndKeepsAllSentences
// constructs a single paragraph (no blank lines, so it must go through
// chunkBySentence internally) long enough to need multiple chunks, with
// overlap enabled — same bug as the paragraph-level test above, but on the
// sentence-accumulation path inside chunkBySentence.
func TestChunkPlainTextSentenceSplitOverlapStaysWithinSizeAndKeepsAllSentences(t *testing.T) {
	const size, overlap = 12, 4
	sentences := []string{"第一句话内容。", "第二句话内容。", "第三句话内容。", "第四句话内容。"}
	text := strings.Join(sentences, "")
	got := chunkPlainText(text, size, overlap)

	assertStringsWithinSize(t, got, size)

	joined := strings.Join(got, "")
	for _, s := range sentences {
		if !strings.Contains(joined, s) {
			t.Fatalf("sentence %q lost, got chunks: %v", s, got)
		}
	}
}

func TestChunkPlainTextChineseParagraphsAndSentences(t *testing.T) {
	text := "第一段第一句话。第一段第二句话，继续一些内容。\n\n第二段只有一句话，但是这一句话本身相当长，用来验证句子级别的边界切分是否正常工作而不是简单粗暴地按字符截断。"
	got := chunkPlainText(text, 30, 0)
	if len(got) == 0 {
		t.Fatalf("expected at least one chunk, got none")
	}
	for _, c := range got {
		if n := len([]rune(c)); n > 30 {
			t.Fatalf("chunk %q has %d runes, want <= 30", c, n)
		}
	}
	// 中文标点作为句子边界：第一段应当被句子而不是硬截断切开。
	joined := strings.Join(got, "")
	if !strings.Contains(joined, "第一段第一句话。") {
		t.Fatalf("sentence boundary broken, got chunks: %v", got)
	}
}

func TestChunkPlainTextParagraphBoundaryPreferredOverMidParagraphCut(t *testing.T) {
	text := "short one.\n\nshort two."
	got := chunkPlainText(text, 100, 0)
	if len(got) != 1 || got[0] != "short one.\n\nshort two." {
		t.Fatalf("expected both short paragraphs merged into one chunk, got %v", got)
	}
}

func TestChunkPlainTextOversizedParagraphSplitsOnSentences(t *testing.T) {
	text := "First sentence is short. Second sentence is also fairly short. Third one too."
	got := chunkPlainText(text, 40, 0)
	if len(got) < 2 {
		t.Fatalf("expected the oversized paragraph to split into multiple chunks, got %v", got)
	}
	for _, c := range got {
		if n := len([]rune(c)); n > 40 {
			t.Fatalf("chunk %q has %d runes, want <= 40", c, n)
		}
	}
	// No sentence should be cut mid-word: every chunk boundary should
	// land right after a terminator or at a paragraph edge.
	for _, c := range got {
		trimmed := strings.TrimSpace(c)
		last := trimmed[len(trimmed)-1]
		if last != '.' {
			t.Fatalf("chunk %q does not end on a sentence boundary", c)
		}
	}
}

// TestChunkPlainTextStructurelessContentMatchesFlatChunkText pins the
// compatibility contract several DB integration tests rely on: content
// with no blank-line paragraphs and no sentence punctuation must chunk
// identically to the old flat chunkText, because both TestIntegration
// ProcessDocumentHappyPath ("aaaaabbbbbccc", size 5) and several other
// fixtures assert exact chunk counts/content.
func TestChunkPlainTextStructurelessContentMatchesFlatChunkText(t *testing.T) {
	cases := []struct {
		text          string
		size, overlap int
	}{
		{"aaaaabbbbbccc", 5, 0},
		{"aaaaabbbbb", 5, 0},
		{strings.Repeat("a", 200), 5, 0},
		{strings.Repeat("a", 200), 5, 2},
	}
	for _, tc := range cases {
		got := chunkPlainText(tc.text, tc.size, tc.overlap)
		want := chunkText(tc.text, tc.size, tc.overlap)
		if len(got) != len(want) {
			t.Fatalf("chunkPlainText(%q) = %v (len %d), want %v (len %d)", tc.text, got, len(got), want, len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("chunkPlainText(%q)[%d] = %q, want %q", tc.text, i, got[i], want[i])
			}
		}
	}
}

func TestChunkPlainTextInvalidOverlapConfigFallsBackSafely(t *testing.T) {
	text := strings.Repeat("word ", 40)
	// overlap >= size and negative overlap must both degrade to overlap=0
	// (see normalizeChunkParams), not hang or duplicate chunks forever.
	gotOversizeOverlap := chunkPlainText(text, 10, 999)
	gotNoOverlap := chunkPlainText(text, 10, 0)
	if len(gotOversizeOverlap) != len(gotNoOverlap) {
		t.Fatalf("overlap>=size did not fall back to overlap=0: %v vs %v", gotOversizeOverlap, gotNoOverlap)
	}
	gotNegative := chunkPlainText(text, 10, -1)
	if len(gotNegative) != len(gotNoOverlap) {
		t.Fatalf("negative overlap did not fall back to overlap=0: %v vs %v", gotNegative, gotNoOverlap)
	}
}

func TestChunkPlainTextEmptyDocument(t *testing.T) {
	if got := chunkPlainText("", 100, 10); got != nil {
		t.Fatalf("chunkPlainText(\"\") = %v, want nil", got)
	}
	if got := chunkPlainText("   \n\n  \n", 100, 10); got != nil {
		t.Fatalf("chunkPlainText(whitespace-only) = %v, want nil", got)
	}
}

// --- chunkMarkdown: heading/paragraph/list/code/table aware chunking ---

func TestChunkMarkdownSectionTitleTracksNearestHeading(t *testing.T) {
	text := "# Guide\n\nIntro text.\n\n## Setup\n\nSetup text.\n\n### Install\n\nInstall text."
	got := chunkMarkdown(text, 200, 0)
	if len(got) != 3 {
		t.Fatalf("expected 3 chunks (one per section), got %d: %+v", len(got), got)
	}
	wantTitles := []string{"Guide", "Setup", "Install"}
	for i, c := range got {
		if c.SectionTitle == nil || *c.SectionTitle != wantTitles[i] {
			t.Fatalf("chunk %d SectionTitle = %v, want %q", i, c.SectionTitle, wantTitles[i])
		}
	}
	// Heading breadcrumb rides along in the embedding content.
	if !strings.Contains(got[2].Content, "Guide > Setup > Install") {
		t.Fatalf("chunk 2 content missing full breadcrumb: %q", got[2].Content)
	}
	if !strings.Contains(got[2].Content, "Install text.") {
		t.Fatalf("chunk 2 content missing body: %q", got[2].Content)
	}
}

func TestChunkMarkdownContentBeforeFirstHeadingHasNilSectionTitle(t *testing.T) {
	text := "Some preamble text with no heading above it.\n\n# First Heading\n\nBody."
	got := chunkMarkdown(text, 200, 0)
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %+v", len(got), got)
	}
	if got[0].SectionTitle != nil {
		t.Fatalf("preamble chunk SectionTitle = %v, want nil", got[0].SectionTitle)
	}
	if strings.Contains(got[0].Content, ">") {
		t.Fatalf("preamble chunk should carry no breadcrumb prefix: %q", got[0].Content)
	}
}

func TestChunkMarkdownSiblingHeadingResetsDeeperLevel(t *testing.T) {
	text := "# A\n\n## A1\n\ntext one.\n\n## A2\n\ntext two."
	got := chunkMarkdown(text, 200, 0)
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %+v", len(got), got)
	}
	if *got[0].SectionTitle != "A1" || !strings.Contains(got[0].Content, "A > A1") {
		t.Fatalf("chunk 0 = %+v, want section A1 under A > A1", got[0])
	}
	if *got[1].SectionTitle != "A2" || !strings.Contains(got[1].Content, "A > A2") {
		t.Fatalf("chunk 1 = %+v, want section A2 under A > A2 (sibling must replace A1, not nest under it)", got[1])
	}
}

func TestChunkMarkdownCodeBlockKeptIntactWhenItFits(t *testing.T) {
	text := "# Example\n\n```go\nfunc main() {\n\tprintln(\"hi\")\n}\n```\n\nAfter code."
	got := chunkMarkdown(text, 500, 0)
	var found bool
	for _, c := range got {
		if strings.Contains(c.Content, "```go") && strings.Contains(c.Content, "```\n\nAfter code.") {
			found = true
		}
		if strings.Contains(c.Content, "```go") && strings.Count(c.Content, "```") != 2 {
			t.Fatalf("code fence split across chunks: %q", c.Content)
		}
	}
	if !found {
		t.Fatalf("expected the fenced code block content preserved verbatim in some chunk: %+v", got)
	}
}

// TestChunkMarkdownOversizedCodeBlockFallsBackToFixedLength is the
// regression test for the reported "breadcrumb added after the fallback
// split, so Content can exceed size" bug: the fallback used to split the
// code block against the full size and only then prepend the breadcrumb
// on top, so every fallback chunk could overflow by len(breadcrumb)+2.
// The fix splits against size-minus-breadcrumb-room instead (see
// chunkMarkdown's oversized-block branch), so every chunk — breadcrumb
// included — must now stay strictly within size, with no fixed slack.
func TestChunkMarkdownOversizedCodeBlockFallsBackToFixedLength(t *testing.T) {
	const size, overlap = 40, 5
	body := strings.Repeat("x = 1\n", 50) // way over size
	text := "# Big\n\n```\n" + body + "```\n"
	got := chunkMarkdown(text, size, overlap)
	if len(got) < 2 {
		t.Fatalf("expected the oversized code block to be split via fallback, got %d chunks", len(got))
	}
	assertChunksWithinSize(t, got, size)

	// The code content itself must not be silently dropped by the
	// fallback split — only allowed to be spread across multiple chunks.
	var allContent string
	for _, c := range got {
		allContent += c.Content
	}
	if !strings.Contains(allContent, "x = 1") {
		t.Fatalf("code content lost in fallback split, got: %+v", got)
	}
}

// TestChunkMarkdownOversizedTableFallbackKeepsAllRowsWithinSize is
// TestChunkMarkdownOversizedCodeBlockFallsBackToFixedLength's table
// counterpart — same fallback path, same invariant, plus an explicit
// check that the first and last row survive the split (the fallback is
// allowed to break the table apart, per the plan, but not to drop rows).
func TestChunkMarkdownOversizedTableFallbackKeepsAllRowsWithinSize(t *testing.T) {
	const size = 60
	var rows strings.Builder
	rows.WriteString("| Col |\n|---|\n")
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&rows, "| row-%d-with-some-extra-padding-text |\n", i)
	}
	text := "# Data\n\n" + rows.String()
	got := chunkMarkdown(text, size, 0)
	if len(got) < 2 {
		t.Fatalf("expected oversized table to fall back to a multi-chunk split, got %d chunks", len(got))
	}
	assertChunksWithinSize(t, got, size)

	var allContent string
	for _, c := range got {
		allContent += c.Content
	}
	if !strings.Contains(allContent, "row-0-") {
		t.Fatalf("first table row lost in fallback split, got: %+v", got)
	}
	if !strings.Contains(allContent, "row-49-") {
		t.Fatalf("last table row lost in fallback split, got: %+v", got)
	}
}

// TestChunkMarkdownBlockOverlapNeverExceedsSizeAndKeepsLaterBlockBody is
// the Markdown counterpart of TestChunkPlainTextOverlapNeverExceedsSize
// AndKeepsBothParagraphs: two ordinary (non-oversized) blocks in the same
// section, sized so the second block's pendingOverlap-prefixed content
// used to blow past size once the breadcrumb was counted on top. Here the
// numbers are tight enough that overlap has to be dropped entirely (no
// room left after the breadcrumb + second block's own body) — which is
// exactly the "取消 overlap" requirement, not just "缩短 overlap".
func TestChunkMarkdownBlockOverlapNeverExceedsSizeAndKeepsLaterBlockBody(t *testing.T) {
	const size, overlap = 14, 4
	text := "# Sec\n\nAAAAAAAA\n\nBBBBBBBB" // two 8-rune paragraph blocks under "Sec"
	got := chunkMarkdown(text, size, overlap)

	assertChunksWithinSize(t, got, size)
	if len(got) < 2 {
		t.Fatalf("expected the two blocks to split into separate chunks, got %d: %+v", len(got), got)
	}

	var allContent string
	for _, c := range got {
		allContent += c.Content
	}
	if !strings.Contains(allContent, "AAAAAAAA") {
		t.Fatalf("first block body lost, got: %+v", got)
	}
	if !strings.Contains(allContent, "BBBBBBBB") {
		t.Fatalf("second block body lost, got: %+v", got)
	}
}

// TestChunkMarkdownDeepOrOverlongBreadcrumbNeverPanicsOrOverflows
// constructs a multi-level heading chain deliberately long enough that
// the full "H1 > H2 > H3" breadcrumb alone exceeds the configured size —
// buildBreadcrumbContent must degrade (drop outer levels, then
// rune-truncate the innermost) rather than let Content overflow or panic
// on a negative slice bound.
func TestChunkMarkdownDeepOrOverlongBreadcrumbNeverPanicsOrOverflows(t *testing.T) {
	const size, overlap = 20, 3
	text := "# " + strings.Repeat("很长的一级标题", 5) +
		"\n\n## " + strings.Repeat("很长的二级标题", 5) +
		"\n\n### " + strings.Repeat("很长的三级标题", 5) +
		"\n\n正文内容。"

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("chunkMarkdown panicked on an overlong breadcrumb: %v", r)
		}
	}()
	got := chunkMarkdown(text, size, overlap)

	if len(got) == 0 {
		t.Fatalf("expected at least one chunk, got none")
	}
	assertChunksWithinSize(t, got, size)
	for i, c := range got {
		if c.Content == "" {
			t.Fatalf("piece %d has empty Content", i)
		}
		if c.SectionTitle == nil || *c.SectionTitle == "" {
			t.Fatalf("piece %d has no reliable SectionTitle: %+v", i, c)
		}
	}
}

// TestChunkMarkdownHeadingOnlyDocumentStillProducesRetrievableChunk is the
// regression test for the reported "只有标题没有正文时被当成空文档" bug:
// a document that's nothing but headings must still produce at least one
// chunk (service.go's ProcessDocument treats zero chunks as
// ErrEmptyContent, which a heading-only document is not), sized safely,
// with a real SectionTitle.
func TestChunkMarkdownHeadingOnlyDocumentStillProducesRetrievableChunk(t *testing.T) {
	const size = 100
	text := "# 安装说明\n## Linux\n"
	got := chunkMarkdown(text, size, 0)

	if len(got) == 0 {
		t.Fatalf("expected at least one chunk from a heading-only document, got none")
	}
	assertChunksWithinSize(t, got, size)
	if got[0].SectionTitle == nil || *got[0].SectionTitle != "Linux" {
		t.Fatalf("SectionTitle = %v, want the innermost heading %q", got[0].SectionTitle, "Linux")
	}
	if !strings.Contains(got[0].Content, "安装说明") || !strings.Contains(got[0].Content, "Linux") {
		t.Fatalf("expected fallback content to include both headings, got %q", got[0].Content)
	}
}

// TestChunkMarkdownHeadingOnlyDocumentOversizedFallsBackWithinSize is the
// heading-only case's size-safety edge: when the joined heading text
// itself doesn't fit in one chunk, the fallback must still split it via
// chunkText rather than emit an oversized single chunk.
func TestChunkMarkdownHeadingOnlyDocumentOversizedFallsBackWithinSize(t *testing.T) {
	const size = 10
	text := "# " + strings.Repeat("Heading", 5) + "\n## " + strings.Repeat("Sub", 5) + "\n"
	got := chunkMarkdown(text, size, 0)

	if len(got) == 0 {
		t.Fatalf("expected at least one chunk from an oversized heading-only document, got none")
	}
	assertChunksWithinSize(t, got, size)
}

// TestChunkMarkdownChunkSizeOneHeadingAndBodyStaysWithinSize is the
// regression test for the reported chunk_size=1 bug: bodyBudget()'s
// "at least half of size" floor used to be a bare `size/2`, which rounds
// down to 0 when size=1. A budget of 0 (or less) reaching chunkText gets
// reinterpreted by chunkText's own normalizeChunkParams as "unconfigured"
// and silently reset to defaultChunkSize (500) — so a knowledge base
// explicitly configured with chunk_size=1 could produce chunks up to 500
// runes. The fix clamps the floor to at least 1.
func TestChunkMarkdownChunkSizeOneHeadingAndBodyStaysWithinSize(t *testing.T) {
	const size = 1
	body := "正文内容一。正文内容二。"
	text := "# 标题\n\n" + body
	got := chunkMarkdown(text, size, 0)

	if len(got) == 0 {
		t.Fatalf("expected at least one chunk, got none")
	}
	assertChunksWithinSize(t, got, size)
	for i, c := range got {
		if c.Content == "" {
			t.Fatalf("piece %d has empty Content", i)
		}
	}

	// No content lost: with overlap=0 there's nothing to deduplicate, so
	// concatenating every piece's Content in order must reconstruct the
	// original body text exactly. This body has no whitespace, so the one
	// thing chunkText is legitimately allowed to skip (a whitespace-only
	// 1-rune window, see TestChunkTextEdgeCases) never applies here — any
	// missing rune would mean real content got dropped, not just a space.
	var rebuilt strings.Builder
	for _, c := range got {
		rebuilt.WriteString(c.Content)
	}
	if rebuilt.String() != body {
		t.Fatalf("body content lost, reordered, or contaminated with breadcrumb text at chunk_size=1: got %q, want %q", rebuilt.String(), body)
	}
}

// TestChunkMarkdownChunkSizeOneOversizedHeadingAndBodyStaysWithinSize is
// TestChunkMarkdownChunkSizeOneHeadingAndBodyStaysWithinSize's harsher
// sibling: a heading long enough on its own to dominate any non-floored
// budget calculation, to make sure the size=1 floor fix holds even when
// the breadcrumb pressure it's meant to absorb is large, not just present.
func TestChunkMarkdownChunkSizeOneOversizedHeadingAndBodyStaysWithinSize(t *testing.T) {
	const size = 1
	body := strings.Repeat("超长正文内容", 10)
	text := "# " + strings.Repeat("超长标题", 10) + "\n\n" + body
	got := chunkMarkdown(text, size, 0)

	if len(got) == 0 {
		t.Fatalf("expected at least one chunk, got none")
	}
	assertChunksWithinSize(t, got, size)
	for i, c := range got {
		if c.Content == "" {
			t.Fatalf("piece %d has empty Content", i)
		}
	}

	var rebuilt strings.Builder
	for _, c := range got {
		rebuilt.WriteString(c.Content)
	}
	if rebuilt.String() != body {
		t.Fatalf("body content lost, reordered, or contaminated with breadcrumb text at chunk_size=1: got %q, want %q", rebuilt.String(), body)
	}
}

func TestChunkMarkdownTableKeptIntactWhenItFits(t *testing.T) {
	text := "# Data\n\n| Col A | Col B |\n|---|---|\n| 1 | 2 |\n| 3 | 4 |\n\nAfter table."
	got := chunkMarkdown(text, 500, 0)
	var found bool
	for _, c := range got {
		if strings.Contains(c.Content, "| Col A | Col B |") && strings.Contains(c.Content, "| 3 | 4 |") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected table preserved intact in some chunk: %+v", got)
	}
}

func TestChunkMarkdownOversizedTableFallsBackToFixedLength(t *testing.T) {
	var rows strings.Builder
	rows.WriteString("| Col |\n|---|\n")
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&rows, "| row-%d-with-some-extra-padding-text |\n", i)
	}
	text := "# Data\n\n" + rows.String()
	got := chunkMarkdown(text, 60, 0)
	if len(got) < 2 {
		t.Fatalf("expected oversized table to fall back to a multi-chunk split, got %d chunks", len(got))
	}
}

func TestChunkMarkdownListKeptIntactWhenItFits(t *testing.T) {
	text := "# Steps\n\n- first step\n- second step\n- third step\n\nDone."
	got := chunkMarkdown(text, 500, 0)
	var found bool
	for _, c := range got {
		if strings.Contains(c.Content, "- first step") && strings.Contains(c.Content, "- third step") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected list preserved intact in some chunk: %+v", got)
	}
}

// TestChunkMarkdownOverlapNeverCrossesSectionBoundary is the plan's
// explicit "overlap 不得重复整个章节" requirement: construct two adjacent
// sections where, if overlap carried across the heading, the second
// section's first chunk would start with a fragment of the first
// section's unique marker text. It must not.
func TestChunkMarkdownOverlapNeverCrossesSectionBoundary(t *testing.T) {
	text := "# One\n\n" + strings.Repeat("alpha ", 20) +
		"\n\n# Two\n\n" + strings.Repeat("beta ", 20)
	got := chunkMarkdown(text, 50, 20)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 chunks (one per section), got %d", len(got))
	}
	for _, c := range got {
		if *c.SectionTitle == "Two" && strings.Contains(c.Content, "alpha") {
			t.Fatalf("section Two chunk leaked overlap from section One: %q", c.Content)
		}
	}
}

func TestChunkMarkdownOverlapCarriesWithinSameSection(t *testing.T) {
	text := "# One\n\n" + strings.Repeat("alpha beta gamma ", 10)
	got := chunkMarkdown(text, 40, 15)
	if len(got) < 2 {
		t.Fatalf("expected the long section to split into multiple chunks, got %d", len(got))
	}
	// Consecutive chunks within the same section should share some
	// overlapping tail/head text (not be strictly disjoint).
	firstBody := strings.TrimPrefix(got[0].Content, "One\n\n")
	secondBody := strings.TrimPrefix(got[1].Content, "One\n\n")
	tail := tailRunes(firstBody, 10)
	if !strings.Contains(secondBody, strings.TrimSpace(tail)[:min(5, len(strings.TrimSpace(tail)))]) {
		t.Fatalf("expected overlap between consecutive same-section chunks: %q / %q", firstBody, secondBody)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestChunkMarkdownInvalidOverlapConfigFallsBackSafely(t *testing.T) {
	text := "# H\n\n" + strings.Repeat("word ", 40)
	gotOversizeOverlap := chunkMarkdown(text, 10, 999)
	gotNoOverlap := chunkMarkdown(text, 10, 0)
	if len(gotOversizeOverlap) != len(gotNoOverlap) {
		t.Fatalf("overlap>=size did not fall back to overlap=0: %d vs %d chunks", len(gotOversizeOverlap), len(gotNoOverlap))
	}
}

func TestChunkMarkdownEmptyDocument(t *testing.T) {
	if got := chunkMarkdown("", 100, 10); got != nil {
		t.Fatalf("chunkMarkdown(\"\") = %+v, want nil", got)
	}
	if got := chunkMarkdown("   \n\n  \n", 100, 10); got != nil {
		t.Fatalf("chunkMarkdown(whitespace-only) = %+v, want nil", got)
	}
}

// --- chunkDocument dispatch ---

func TestChunkDocumentDispatchesByFileType(t *testing.T) {
	md := chunkDocument(FileTypeMD, parsedContent{Text: "# H\n\nbody"}, 200, 0)
	if len(md) != 1 || md[0].SectionTitle == nil {
		t.Fatalf("md dispatch = %+v, want one chunk with a section title", md)
	}
	txt := chunkDocument(FileTypeTxt, parsedContent{Text: "plain body"}, 200, 0)
	if len(txt) != 1 || txt[0].SectionTitle != nil || txt[0].PageNumber != nil {
		t.Fatalf("txt dispatch = %+v, want one chunk with no metadata", txt)
	}
	pdfPieces := chunkDocument(FileTypePDF, parsedContent{Pages: []pdfPage{{Number: 1, Text: "page one body"}}}, 200, 0)
	if len(pdfPieces) != 1 || pdfPieces[0].PageNumber == nil || *pdfPieces[0].PageNumber != 1 {
		t.Fatalf("pdf dispatch = %+v, want one chunk tagged page 1", pdfPieces)
	}
}

// --- chunkPDFPages: per-page chunking, never mixing pages ---

func TestChunkPDFPagesTagsEachPieceWithItsOwnPage(t *testing.T) {
	pages := []pdfPage{
		{Number: 1, Text: strings.Repeat("alpha ", 20)},
		{Number: 2, Text: strings.Repeat("beta ", 20)},
	}
	got := chunkPDFPages(pages, 30, 10)
	if len(got) < 2 {
		t.Fatalf("expected multiple chunks across 2 pages, got %d", len(got))
	}
	for _, c := range got {
		if c.PageNumber == nil {
			t.Fatalf("chunk missing page number: %+v", c)
		}
		switch *c.PageNumber {
		case 1:
			if strings.Contains(c.Content, "beta") {
				t.Fatalf("page 1 chunk contaminated with page 2 content: %q", c.Content)
			}
		case 2:
			if strings.Contains(c.Content, "alpha") {
				t.Fatalf("page 2 chunk contaminated with page 1 content: %q", c.Content)
			}
		default:
			t.Fatalf("unexpected page number %d", *c.PageNumber)
		}
	}
}

func TestChunkPDFPagesSkipsBlankPages(t *testing.T) {
	pages := []pdfPage{
		{Number: 1, Text: "real content here"},
		{Number: 2, Text: "   "},
		{Number: 3, Text: "more real content"},
	}
	got := chunkPDFPages(pages, 200, 0)
	for _, c := range got {
		if *c.PageNumber == 2 {
			t.Fatalf("blank page 2 must not produce a chunk: %+v", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 chunks (pages 1 and 3), got %d: %+v", len(got), got)
	}
}

func TestChunkPDFPagesAllBlankProducesNoChunks(t *testing.T) {
	pages := []pdfPage{{Number: 1, Text: ""}, {Number: 2, Text: "  \n "}}
	if got := chunkPDFPages(pages, 200, 0); got != nil {
		t.Fatalf("chunkPDFPages(all blank) = %+v, want nil (scanned-PDF case, must surface as empty content upstream)", got)
	}
}

// --- extractPDFPages: real PDF bytes, hand-built (see writeTestPDF) ---

func TestExtractPDFPagesReturnsTextPerPageWithCorrectNumbers(t *testing.T) {
	path := writeTestPDF(t, []string{
		"This is page one content for testing",
		"This is page two content different from one",
		"This is page three the final page here",
	})

	pages, err := extractPDFPages(path)
	if err != nil {
		t.Fatalf("extractPDFPages: %v", err)
	}
	if len(pages) != 3 {
		t.Fatalf("got %d pages, want 3", len(pages))
	}
	wantSubstrings := []string{"page one", "page two", "page three"}
	for i, p := range pages {
		if p.Number != i+1 {
			t.Fatalf("page %d has Number=%d, want %d", i, p.Number, i+1)
		}
		if !strings.Contains(p.Text, wantSubstrings[i]) {
			t.Fatalf("page %d text = %q, want it to contain %q", p.Number, p.Text, wantSubstrings[i])
		}
	}
	// Cross-page isolation: page 1's text must not leak "page two"/"page three".
	if strings.Contains(pages[0].Text, "page two") || strings.Contains(pages[0].Text, "page three") {
		t.Fatalf("page 1 text contaminated with other pages: %q", pages[0].Text)
	}
}

func TestParseFileAndChunkPDFEndToEndPageNumbersNeverCrossed(t *testing.T) {
	path := writeTestPDF(t, []string{
		strings.Repeat("alphaword ", 15),
		strings.Repeat("betaword ", 15),
	})

	parsed, err := parseFile(path, FileTypePDF)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	pieces := chunkDocument(FileTypePDF, parsed, 30, 5)
	if len(pieces) == 0 {
		t.Fatalf("expected chunks from a 2-page pdf, got none")
	}
	for _, p := range pieces {
		if p.PageNumber == nil {
			t.Fatalf("pdf chunk missing page number: %+v", p)
		}
		if p.SectionTitle != nil {
			t.Fatalf("pdf chunk must never fabricate a section title: %+v", p)
		}
		hasAlpha := strings.Contains(p.Content, "alphaword")
		hasBeta := strings.Contains(p.Content, "betaword")
		if hasAlpha && hasBeta {
			t.Fatalf("chunk spans both pages, page number would be unreliable: %+v", p)
		}
		if hasAlpha && *p.PageNumber != 1 {
			t.Fatalf("alpha content tagged with page %d, want 1", *p.PageNumber)
		}
		if hasBeta && *p.PageNumber != 2 {
			t.Fatalf("beta content tagged with page %d, want 2", *p.PageNumber)
		}
	}
}

func TestParseFileScannedPDFWithNoTextYieldsEmptyContent(t *testing.T) {
	// A page with no text-showing operators at all — the scanned-PDF case
	// (no OCR in this phase, see parse.go's doc comment).
	path := writeTestPDF(t, []string{""})

	parsed, err := parseFile(path, FileTypePDF)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	pieces := chunkDocument(FileTypePDF, parsed, 200, 0)
	if pieces != nil {
		t.Fatalf("expected no chunks from a textless pdf, got %+v", pieces)
	}
}

// --- writeTestPDF: minimal hand-built single-column-text PDF for tests ---
//
// rsc.io/pdf has no companion writer, and there's no PDF-generation
// dependency in go.mod — so this builds the smallest classic (non-stream-
// xref) PDF that satisfies rsc.io/pdf's reader: one indirect object per
// page/content-stream/font, a plain (not compressed) xref table, and a
// fixed-pitch Courier font with an explicit Widths array (so glyph
// advances are non-zero and extractPDFPages's gap-based word-spacing
// heuristic has real X deltas to work with). Callers must stick to plain
// ASCII words with no '(', ')', or '\\' — those would need PDF string
// escaping this helper doesn't implement, and no test needs it.
func writeTestPDF(t *testing.T, pages []string) string {
	t.Helper()
	n := len(pages)
	fontObj := 3 + 2*n
	totalObjs := fontObj

	var buf bytes.Buffer
	offsets := make([]int, totalObjs+1)

	buf.WriteString("%PDF-1.4\n")

	writeObj := func(id int, body string) {
		offsets[id] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", id, body)
	}

	var kids strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&kids, "%d 0 R ", 3+i)
	}

	writeObj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObj(2, fmt.Sprintf(
		"<< /Type /Pages /Kids [ %s] /Count %d /MediaBox [0 0 612 792] /Resources << /Font << /F1 %d 0 R >> >> >>",
		kids.String(), n, fontObj))

	for i, pageText := range pages {
		pageID := 3 + i
		contentID := 3 + n + i
		writeObj(pageID, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /Contents %d 0 R >>", contentID))

		// The whole page's text goes out as a single Tj string on one
		// line — reconstructing line breaks isn't what these tests
		// exercise (chunkPlainText's paragraph splitting is already
		// covered directly against strings), only that spaces within a
		// line and page boundaries survive extractPDFPages correctly.
		var content strings.Builder
		if trimmed := strings.TrimSpace(pageText); trimmed != "" {
			fmt.Fprintf(&content, "BT /F1 12 Tf 72 700 Td (%s) Tj ET\n", trimmed)
		}
		contentBytes := content.String()
		offsets[contentID] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n<< /Length %d >>\nstream\n%sendstream\nendobj\n", contentID, len(contentBytes), contentBytes)
	}

	widths := make([]string, 95) // codes 32..126, fixed-pitch Courier
	for i := range widths {
		widths[i] = "600"
	}
	writeObj(fontObj, fmt.Sprintf(
		"<< /Type /Font /Subtype /Type1 /BaseFont /Courier /FirstChar 32 /LastChar 126 /Widths [ %s ] >>",
		strings.Join(widths, " ")))

	xrefOffset := buf.Len()
	total := totalObjs + 1
	fmt.Fprintf(&buf, "xref\n0 %d\n", total)
	buf.WriteString("0000000000 65535 f \n")
	for id := 1; id < total; id++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[id])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", total, xrefOffset)

	path := t.TempDir() + "/test.pdf"
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write test pdf: %v", err)
	}
	return path
}
