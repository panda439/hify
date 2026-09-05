package knowledge

import (
	"regexp"
	"strings"
)

// chunkText splits text into overlapping chunks of at most size runes,
// stepping by (size - overlap) each time. Operates on runes, not bytes —
// Hify's content is often Chinese, where byte-slicing would cut a
// multi-byte UTF-8 character in half.
//
// This is the fixed-length fallback engine every structure-aware chunker
// below (chunkMarkdown/chunkPlainText/chunkPDFPages) drops down to when a
// single structural unit (a code block, a table, a paragraph, a sentence
// with no punctuation) is larger than size on its own — see each of their
// doc comments for exactly when that happens.
func chunkText(text string, size, overlap int) []string {
	size, overlap = normalizeChunkParams(size, overlap)

	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}

	step := size - overlap
	var chunks []string
	for start := 0; start < len(runes); start += step {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		if chunk := strings.TrimSpace(string(runes[start:end])); chunk != "" {
			chunks = append(chunks, chunk)
		}
		if end == len(runes) {
			break
		}
	}
	return chunks
}

// normalizeChunkParams is the one place size/overlap get sanity-checked —
// every chunker in this file (chunkText, chunkMarkdown, chunkPlainText,
// chunkBySentence) calls it first so a misconfigured knowledge base
// (chunk_size <= 0, overlap negative or >= size) degrades the same way
// everywhere instead of each function reinventing (and potentially
// disagreeing on) the fallback.
func normalizeChunkParams(size, overlap int) (int, int) {
	if size <= 0 {
		size = defaultChunkSize
	}
	if overlap < 0 || overlap >= size {
		// A misconfigured overlap must not turn stepping into an infinite
		// loop (step would be <= 0) — fall back to no overlap.
		overlap = 0
	}
	return size, overlap
}

// tailRunes returns the last n runes of s (or the whole of s if it has
// fewer than n) — used to build the small "carried forward" overlap seed
// between two size-triggered chunk splits, bounded to strictly less than a
// full chunk's worth of content because overlap < size is already
// guaranteed by normalizeChunkParams.
func tailRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// prependOverlap is the one place chunkMarkdown/chunkPlainText/
// chunkBySentence attach a carried-forward overlap seed ahead of the first
// unit (paragraph/sentence/block) of a new chunk. It exists to fix a bug
// where the overlap seed used to be spliced onto the new content
// unconditionally — overlapTail + sep + content could exceed budget even
// though content alone had already been confirmed to fit, silently
// breaking the "every non-empty output <= chunk_size" invariant (see the
// package doc's example: overlap=4 + sep=1 + a fresh 8-rune paragraph = 13
// runes against a chunk_size of 10).
//
// The fix follows the plan's explicit priority order: content always wins
// (callers only ever reach here after confirming content alone fits within
// budget, so it is never trimmed here); overlap shrinks to whatever room
// is left, keeping the runes closest to content (its own tail, since
// overlapTail is already "the tail of the previous chunk" — the runes
// nearest the join point are the most locally relevant ones to keep); and
// if there's no room at all, overlap is dropped entirely rather than
// pushing content over budget.
func prependOverlap(overlapTail, sep, content string, budget int) string {
	if overlapTail == "" {
		return content
	}
	available := budget - len([]rune(content)) - len([]rune(sep))
	if available <= 0 {
		return content
	}
	return tailRunes(overlapTail, available) + sep + content
}

// chunkPiece is one structure-aware chunk plus whatever source-attribution
// metadata its parser could honestly derive. PageNumber/PageEnd are only
// ever set by the PDF chunker, SectionTitle only ever by chunkMarkdown —
// never fabricate a value the source format can't reliably support (see
// Chunk's doc comment in model.go).
//
// PageNumber and PageEnd are a closed interval (first page covered, last
// page covered), not two independent numbers. Invariants, mirroring
// Chunk's C1-C3 and the chunks_page_range_valid database constraint:
//
//	C1  PageEnd == nil ⟺ PageNumber == nil
//	C2  both set ⇒ *PageNumber <= *PageEnd
//	C3  both set ⇒ 1 <= *PageNumber and *PageEnd <= the document's page count
//	C4  txt/md pieces have BOTH nil, always (FR-014/FR-020)
//
// C1 is why the two fields must be filled in together at every producing
// site: a piece with a page number but no page end is rejected outright by
// the database, and would silently vanish from any page-filtered search if
// it ever got past it.
type chunkPiece struct {
	Content      string
	PageNumber   *int
	PageEnd      *int
	SectionTitle *string
}

// chunkDocument dispatches to the structure-aware chunker for fileType.
// Every branch still bottoms out at chunkText for any single structural
// unit that doesn't fit in size runes — see each chunker's doc comment.
func chunkDocument(fileType string, parsed parsedContent, size, overlap int) []chunkPiece {
	switch fileType {
	case FileTypeMD:
		return chunkMarkdown(parsed.Text, size, overlap)
	case FileTypePDF:
		return chunkPDFPages(parsed.Pages, size, overlap)
	default: // FileTypeTxt
		bodies := chunkPlainText(parsed.Text, size, overlap)
		if len(bodies) == 0 {
			return nil
		}
		pieces := make([]chunkPiece, len(bodies))
		for i, body := range bodies {
			pieces[i] = chunkPiece{Content: body}
		}
		return pieces
	}
}

// --- Markdown: heading/paragraph/list/code-block/table aware chunking ---

type mdBlockKind int

const (
	mdParagraph mdBlockKind = iota
	mdHeading
	mdList
	mdCode
	mdTable
)

type mdBlock struct {
	Kind  mdBlockKind
	Level int // heading level 1-6; unused for other kinds
	Text  string
}

var (
	mdHeadingPattern    = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdListItemPattern   = regexp.MustCompile(`^\s*([-*+]|\d+[.)])\s+`)
	mdTableDelimPattern = regexp.MustCompile(`^\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)*\|?$`)
)

func isFenceLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

// isTableStart requires a GFM-style header+delimiter pair (line i has at
// least one "|", line i+1 is a delimiter row like "|---|---|") — a single
// line containing "|" with no delimiter row underneath is just prose that
// happens to contain a pipe, not a table.
func isTableStart(lines []string, i int) bool {
	if i+1 >= len(lines) || !strings.Contains(lines[i], "|") {
		return false
	}
	return mdTableDelimPattern.MatchString(strings.TrimSpace(lines[i+1]))
}

// parseMarkdownBlocks is a line-based heuristic scanner, not a CommonMark
// parser — it recognizes exactly the structures the plan calls out
// (headings, paragraphs, lists, fenced code blocks, GFM tables) well
// enough to keep them intact through chunking. Nested constructs, Setext
// headings ("Title\n====="), and inline HTML are not specially handled;
// they fall through to plain paragraph blocks, which is a safe (if
// unglamorous) degradation.
func parseMarkdownBlocks(text string) []mdBlock {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	n := len(lines)
	var blocks []mdBlock

	isBoundary := func(j int) bool {
		t := strings.TrimSpace(lines[j])
		return t == "" || mdHeadingPattern.MatchString(t) || isFenceLine(t) ||
			mdListItemPattern.MatchString(lines[j]) || isTableStart(lines, j)
	}

	for i := 0; i < n; {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "":
			i++

		case isFenceLine(trimmed):
			marker := trimmed[:3]
			j := i + 1
			for j < n && !strings.HasPrefix(strings.TrimSpace(lines[j]), marker) {
				j++
			}
			end := n
			if j < n {
				end = j + 1 // include the closing fence line
			}
			blocks = append(blocks, mdBlock{Kind: mdCode, Text: strings.Join(lines[i:end], "\n")})
			i = end

		case mdHeadingPattern.MatchString(trimmed):
			m := mdHeadingPattern.FindStringSubmatch(trimmed)
			blocks = append(blocks, mdBlock{Kind: mdHeading, Level: len(m[1]), Text: strings.TrimSpace(m[2])})
			i++

		case isTableStart(lines, i):
			j := i
			for j < n && strings.TrimSpace(lines[j]) != "" && strings.Contains(lines[j], "|") {
				j++
			}
			blocks = append(blocks, mdBlock{Kind: mdTable, Text: strings.Join(lines[i:j], "\n")})
			i = j

		case mdListItemPattern.MatchString(line):
			j := i + 1
			for j < n && !isBoundary(j) {
				j++
			}
			blocks = append(blocks, mdBlock{Kind: mdList, Text: strings.Join(lines[i:j], "\n")})
			i = j

		default:
			j := i + 1
			for j < n && !isBoundary(j) {
				j++
			}
			blocks = append(blocks, mdBlock{Kind: mdParagraph, Text: strings.Join(lines[i:j], "\n")})
			i = j
		}
	}
	return blocks
}

// buildBreadcrumbContent joins body with a heading breadcrumb prefix,
// shortening or dropping the breadcrumb — never trimming body, which every
// caller (emit, via chunkMarkdown's accumulation/oversized-block budgeting)
// has already confirmed fits within size on its own — so the combined
// result never exceeds size runes. This is the final safety net for the
// "breadcrumb 太长时可以安全缩短或降级，但不能让最终 Content 超限"
// requirement: bodyBudget's max(size/2, 1) floor (see chunkMarkdown) means
// the budget body was sized against can legitimately be less than what the
// full breadcrumb needs, so this has to reconcile the two independently of
// how body was produced, not assume it already leaves exact room.
//
// Degrades in order: the full breadcrumb ("H1 > H2 > H3") if it fits;
// otherwise progressively drop the outermost heading levels, keeping the
// innermost ones (most locally relevant to body); if even the single
// innermost heading alone doesn't fit, rune-truncate it to whatever room
// is left; if there's no room at all, no breadcrumb.
func buildBreadcrumbContent(headingStack []string, body string, size int) string {
	if len(headingStack) == 0 {
		return body
	}
	available := size - len([]rune(body)) - 2 // 2 = the "\n\n" separator
	if available <= 0 {
		return body
	}

	for start := 0; start < len(headingStack); start++ {
		bc := strings.Join(headingStack[start:], " > ")
		if len([]rune(bc)) <= available {
			return bc + "\n\n" + body
		}
	}

	// Even the innermost heading alone (the last loop iteration above,
	// headingStack[len-1:]) didn't fit — truncate it to the room that's
	// left rather than drop section context entirely.
	innermost := []rune(headingStack[len(headingStack)-1])
	if len(innermost) > available {
		innermost = innermost[:available]
	}
	return string(innermost) + "\n\n" + body
}

// chunkMarkdown groups consecutive blocks under their nearest heading into
// chunks of at most size runes, never splitting a code block or table
// unless it alone exceeds size (then it falls back to chunkText — see the
// package doc comment). Two things ride along with every chunk:
//
//   - Content is prefixed with the active heading breadcrumb ("H1 >
//     H2 > ..."), so the text actually sent to the embedding model carries
//     its section context — a chunk about "installation steps" embeds
//     better when it also says "Setup > Installation".
//   - SectionTitle is set to the innermost active heading (nil before the
//     first heading), a reliable, never-fabricated single string for
//     display.
//
// Overlap is carried between two chunks only when the split between them
// was purely a size overflow within the same section — crossing a heading
// boundary always resets it, so overlap can never reproduce a whole
// section (see the plan's explicit requirement).
func chunkMarkdown(text string, size, overlap int) []chunkPiece {
	size, overlap = normalizeChunkParams(size, overlap)
	blocks := parseMarkdownBlocks(text)
	if len(blocks) == 0 {
		return nil
	}

	var pieces []chunkPiece
	var headingStack []string
	var levelStack []int
	var acc []string
	var pendingOverlap string
	// headingTexts records every heading encountered, in document order,
	// independent of headingStack (which pops entries as sibling/higher
	// headings close out a section) — the only consumer is the
	// heading-only-document fallback after the main loop, see below.
	var headingTexts []string

	breadcrumb := func() string { return strings.Join(headingStack, " > ") }
	currentSectionTitle := func() *string {
		if len(headingStack) == 0 {
			return nil
		}
		t := headingStack[len(headingStack)-1]
		return &t
	}
	accRuneLen := func() int {
		if len(acc) == 0 {
			return 0
		}
		total := 2 * (len(acc) - 1) // "\n\n" between accumulated blocks
		for _, s := range acc {
			total += len([]rune(s))
		}
		return total
	}
	bodyBudget := func() int {
		bc := breadcrumb()
		if bc == "" {
			return size
		}
		b := size - len([]rune(bc)) - 2
		// floor is "at least half of size" so a deeply nested heading
		// path can't crowd out virtually all real content — the
		// breadcrumb prefix is a retrieval aid, not worth starving the
		// body for. This can leave less room than the full breadcrumb
		// actually needs; buildBreadcrumbContent (in emit) is the final
		// safety net that reconciles the two without ever exceeding
		// size, dropping the breadcrumb entirely when there's no room
		// (body always wins).
		//
		// size/2 itself must never be used as-is: at size=1 it rounds
		// down to 0, and a budget of 0 (or less) reaching chunkText below
		// gets reinterpreted by chunkText's own normalizeChunkParams as
		// "unconfigured" and reset to defaultChunkSize (500) — silently
		// blowing a chunk_size=1 knowledge base up to 500-rune chunks.
		// The floor is clamped to at least 1 so body always gets a real,
		// positive budget regardless of how small size is.
		floor := size / 2
		if floor < 1 {
			floor = 1
		}
		if b < floor {
			b = floor
		}
		return b
	}
	emit := func(body string) {
		content := buildBreadcrumbContent(headingStack, body, size)
		pieces = append(pieces, chunkPiece{Content: content, SectionTitle: currentSectionTitle()})
	}
	// flush closes out the current accumulator as one chunk. carryOverlap
	// controls whether the tail of this chunk seeds the next one —
	// callers pass false at every section/oversized-block boundary so
	// overlap never crosses those boundaries (an empty acc still clears
	// any stale pendingOverlap in that case, otherwise a same-section
	// overlap seed could leak past a heading it should have died at).
	flush := func(carryOverlap bool) {
		if len(acc) == 0 {
			if !carryOverlap {
				pendingOverlap = ""
			}
			return
		}
		body := strings.Join(acc, "\n\n")
		emit(body)
		if carryOverlap && overlap > 0 {
			pendingOverlap = tailRunes(body, overlap)
		} else {
			pendingOverlap = ""
		}
		acc = nil
	}

	for _, block := range blocks {
		if block.Kind == mdHeading {
			flush(false)
			headingTexts = append(headingTexts, block.Text)
			for len(levelStack) > 0 && levelStack[len(levelStack)-1] >= block.Level {
				levelStack = levelStack[:len(levelStack)-1]
				headingStack = headingStack[:len(headingStack)-1]
			}
			levelStack = append(levelStack, block.Level)
			headingStack = append(headingStack, block.Text)
			continue
		}

		blockLen := len([]rune(block.Text))
		budget := bodyBudget()

		if blockLen > budget {
			// A single structure (usually a code block or table) too big
			// to fit even alone — the only safe move is the fixed-length
			// fallback, per the plan. Isolated from the surrounding
			// accumulator/overlap bookkeeping on purpose. Split against
			// budget, not size — budget is size minus what the active
			// breadcrumb needs (see bodyBudget), so each sub-piece still
			// has room for the breadcrumb emit() is about to prepend. This
			// was the bug: splitting against the full size and then adding
			// the breadcrumb on top let the final Content exceed size.
			flush(false)
			for _, sub := range chunkText(block.Text, budget, overlap) {
				emit(sub)
			}
			continue
		}

		if len(acc) > 0 && accRuneLen()+2+blockLen > budget {
			flush(true)
		}

		next := block.Text
		if len(acc) == 0 && pendingOverlap != "" {
			next = prependOverlap(pendingOverlap, "\n", block.Text, budget)
			pendingOverlap = ""
		}
		acc = append(acc, next)
	}
	flush(false)

	if len(pieces) == 0 && len(headingTexts) > 0 {
		// Heading-only document (e.g. "# 安装说明\n## Linux" with no body
		// text under either heading at all) — the main loop above never
		// calls emit because emit only ever fires from flush(body-not-
		// empty) or the oversized-block path, neither of which a pure
		// sequence of heading blocks ever reaches. Headings alone are
		// still meaningful, retrievable content (a breadcrumb like "Setup
		// > Installation" is searchable on its own), so surface them as
		// one fallback chunk instead of silently returning zero chunks —
		// service.go treats zero chunks as ErrEmptyContent, which a
		// heading-only document is not.
		fallback := strings.Join(headingTexts, " > ")
		title := headingTexts[len(headingTexts)-1]
		for _, sub := range chunkText(fallback, size, overlap) {
			pieces = append(pieces, chunkPiece{Content: sub, SectionTitle: &title})
		}
	}

	return pieces
}

// --- Plain text: paragraph/sentence aware chunking ---

// blankLinePattern splits text into paragraphs on one or more blank
// lines — CRLF is normalized to LF first so a lone \r doesn't defeat the
// match.
var blankLinePattern = regexp.MustCompile(`\n\s*\n+`)

func splitParagraphs(text string) []string {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	var out []string
	for _, p := range blankLinePattern.Split(normalized, -1) {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// sentenceBoundaryPattern captures a run of non-terminator characters plus
// its trailing terminator(s) — covers both Chinese (。！？) and ASCII
// (.!?) sentence endings, which is all chunkBySentence needs to find a
// safer place to cut than an arbitrary rune offset.
var sentenceBoundaryPattern = regexp.MustCompile(`[^。！？.!?]*[。！？.!?]+`)

func splitSentences(text string) []string {
	idxs := sentenceBoundaryPattern.FindAllStringIndex(text, -1)
	var out []string
	last := 0
	for _, m := range idxs {
		last = m[1]
		if t := strings.TrimSpace(text[m[0]:m[1]]); t != "" {
			out = append(out, t)
		}
	}
	if rest := strings.TrimSpace(text[last:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

// chunkBySentence groups a paragraph's sentences into chunks of at most
// size runes — called only when the whole paragraph didn't fit on its
// own. A single sentence that's still too big (no punctuation at all, the
// common case for content with no real sentence structure) falls back to
// chunkText, which is what keeps chunkPlainText byte-for-byte compatible
// with the old flat chunker for structureless content.
func chunkBySentence(text string, size, overlap int) []string {
	sentences := splitSentences(text)
	if len(sentences) == 0 {
		return nil
	}

	var chunks []string
	var acc []string
	var pendingOverlap string

	accRuneLen := func() int {
		if len(acc) == 0 {
			return 0
		}
		total := len(acc) - 1 // single space between accumulated sentences
		for _, s := range acc {
			total += len([]rune(s))
		}
		return total
	}
	flush := func(carryOverlap bool) {
		if len(acc) == 0 {
			if !carryOverlap {
				pendingOverlap = ""
			}
			return
		}
		body := strings.Join(acc, " ")
		if t := strings.TrimSpace(body); t != "" {
			chunks = append(chunks, t)
		}
		if carryOverlap && overlap > 0 {
			pendingOverlap = tailRunes(body, overlap)
		} else {
			pendingOverlap = ""
		}
		acc = nil
	}

	for _, sentence := range sentences {
		sLen := len([]rune(sentence))
		if sLen > size {
			flush(false)
			chunks = append(chunks, chunkText(sentence, size, overlap)...)
			continue
		}

		if len(acc) > 0 && accRuneLen()+1+sLen > size {
			flush(true)
		}

		next := sentence
		if len(acc) == 0 && pendingOverlap != "" {
			next = prependOverlap(pendingOverlap, " ", sentence, size)
			pendingOverlap = ""
		}
		acc = append(acc, next)
	}
	flush(false)
	return chunks
}

// chunkPlainText splits TXT content on paragraph boundaries first, then
// sentence boundaries within an oversized paragraph, only falling back to
// a fixed-length chunkText split when even a single sentence doesn't fit
// in size runes. For content with no blank-line paragraphs and no
// sentence-ending punctuation at all (a single run-on blob), every level
// degrades to exactly one paragraph and one "sentence" covering the whole
// text, so the final fallback reproduces the old flat chunkText behavior
// exactly — existing knowledge bases built on such content keep chunking
// identically.
func chunkPlainText(text string, size, overlap int) []string {
	size, overlap = normalizeChunkParams(size, overlap)
	paragraphs := splitParagraphs(text)
	if len(paragraphs) == 0 {
		return nil
	}

	var chunks []string
	var acc []string
	var pendingOverlap string

	accRuneLen := func() int {
		if len(acc) == 0 {
			return 0
		}
		total := 2 * (len(acc) - 1) // blank line between accumulated paragraphs
		for _, s := range acc {
			total += len([]rune(s))
		}
		return total
	}
	flush := func(carryOverlap bool) {
		if len(acc) == 0 {
			if !carryOverlap {
				pendingOverlap = ""
			}
			return
		}
		body := strings.Join(acc, "\n\n")
		if t := strings.TrimSpace(body); t != "" {
			chunks = append(chunks, t)
		}
		if carryOverlap && overlap > 0 {
			pendingOverlap = tailRunes(body, overlap)
		} else {
			pendingOverlap = ""
		}
		acc = nil
	}

	for _, para := range paragraphs {
		paraLen := len([]rune(para))
		if paraLen > size {
			flush(false)
			chunks = append(chunks, chunkBySentence(para, size, overlap)...)
			continue
		}

		if len(acc) > 0 && accRuneLen()+2+paraLen > size {
			flush(true)
		}

		next := para
		if len(acc) == 0 && pendingOverlap != "" {
			next = prependOverlap(pendingOverlap, "\n", para, size)
			pendingOverlap = ""
		}
		acc = append(acc, next)
	}
	flush(false)
	return chunks
}

// --- PDF: paragraph-stream chunking, pages are coordinates not boundaries ---

// chunkPDFPages chunks a PDF by consuming the reassembled paragraph stream
// (layout.go's buildParagraphStream) rather than one page at a time, and
// tags every piece with the closed page interval it actually covers.
//
// What changed and why (006-pdf-layout-chunking). The previous version
// chunked each page independently and gave each piece that page's number.
// The doc comment defended this as "the only way to guarantee PageNumber
// is never wrong" — which was true, and beside the point: it bought an
// always-correct page number by making the CHUNKS wrong. A paragraph
// straddling a page break came out as two half-paragraphs, each missing
// the context that made it meaningful, neither similar enough to the
// user's question to be retrieved, and the one that did surface ended
// mid-sentence and invited the model to complete it. Nothing downstream
// could repair that: the neighbour window can attach the adjacent chunk,
// but it cannot un-cut the sentence. Carrying a page INTERVAL keeps the
// citation just as honest without paying that price.
//
// Overlap now carries across a page break as a side effect, not as a
// special case: overlap was per-call state in chunkPlainText and every
// page was its own call, so a page boundary was the one place in the whole
// pipeline with no overlap protection at all. One call over the whole
// stream removes that hole without any code that knows about it.
//
// Length limits are untouched (FR-004): a merged paragraph longer than
// size still falls back to chunkBySentence and ultimately to fixed-length
// chunkText, exactly as an over-long single-page paragraph always did.
// Merging changes what counts as a paragraph; it does not exempt anything
// from the size budget.
//
// Pages with no extractable text contribute no units and are skipped, as
// before — and, per buildParagraphStream, they also break continuity
// rather than letting the pages either side of them be joined.
func chunkPDFPages(pages []pdfPage, size, overlap int) []chunkPiece {
	pieces := chunkPDFStream(buildParagraphStream(pages), size, overlap)
	return dropImpossiblePageIntervals(pieces, maxPageNumber(pages))
}

// dropImpossiblePageIntervals is the last-resort guard on invariant C3's
// upper half — "the interval lies within the document's pages" — which is
// the one part of C1/C2/C3 the database cannot check, because the database
// has no idea how many pages a document has.
//
// If an interval ever comes out impossible, the answer is to report NO page
// rather than a wrong one: a missing citation is visibly missing, while a
// citation pointing at a page that does not exist looks perfectly normal
// until someone goes to check it. Nothing should ever reach here — every
// page number originates from pdfPage.Number — so it also logs nothing and
// costs one comparison per piece; it exists so that a future change that
// breaks the invariant degrades honestly instead of inventing pages.
func dropImpossiblePageIntervals(pieces []chunkPiece, maxPage int) []chunkPiece {
	for i, p := range pieces {
		if p.PageNumber == nil || p.PageEnd == nil {
			continue
		}
		if *p.PageNumber < 1 || *p.PageNumber > *p.PageEnd || *p.PageEnd > maxPage {
			pieces[i].PageNumber, pieces[i].PageEnd = nil, nil
		}
	}
	return pieces
}

func maxPageNumber(pages []pdfPage) int {
	maxPage := 0
	for _, p := range pages {
		if p.Number > maxPage {
			maxPage = p.Number
		}
	}
	return maxPage
}

// chunkPDFStream is chunkPlainText's accumulation loop with page-interval
// bookkeeping alongside it: same paragraph packing, same oversized-unit
// fallback, same overlap budget rules, but every emitted piece also
// reports the span of pages the text in it came from.
//
// ⭐ Page attribution rule: a piece's interval is the union of the
// intervals of the units that contributed text to it — INCLUDING the unit
// that contributed only an overlap seed. The alternative (count only "the
// unit's own" text) was rejected: the seed is genuinely present in the
// chunk, and FR-010 asks for the pages a chunk actually covers. Widening
// an interval is not fabrication; narrowing it is, because it sends a
// reader to page 4 to find a sentence printed on page 3.
//
// ⚠️ The cost, stated plainly rather than buried: a chunk whose body is
// entirely on page 4 but whose overlap seed came from page 3 is labelled
// "3-4". That is intended, not a defect — but it does mean intervals are
// slightly wider than the strict minimum wherever overlap crosses a page.
// Likewise, when one merged unit is split into several pieces by the
// oversize fallback, every piece inherits the whole unit's interval; the
// split happens inside text this package no longer tracks positions for,
// and guessing which half of a merged paragraph a sentence came from would
// be inventing precision that isn't there.
func chunkPDFStream(units []paragraphUnit, size, overlap int) []chunkPiece {
	size, overlap = normalizeChunkParams(size, overlap)
	if len(units) == 0 {
		return nil
	}

	var pieces []chunkPiece
	var acc []string
	var pendingOverlap string
	// accHeadings is the heading stack shared by everything in acc. Units
	// under a different heading force a flush, so one chunk never carries a
	// breadcrumb that only half of it belongs to.
	var accHeadings []string
	// accStart/accEnd track the interval of everything currently in acc,
	// including any overlap seed already spliced onto its first element.
	// 0 means "nothing accumulated yet".
	accStart, accEnd := 0, 0
	// carryStart/carryEnd is the interval of the pending overlap text, so
	// the seed's pages follow it into the next piece.
	carryStart, carryEnd := 0, 0

	cover := func(start, end int) {
		if start <= 0 || end <= 0 {
			return
		}
		if accStart == 0 || start < accStart {
			accStart = start
		}
		if end > accEnd {
			accEnd = end
		}
	}

	accRuneLen := func() int {
		if len(acc) == 0 {
			return 0
		}
		total := 2 * (len(acc) - 1)
		for _, s := range acc {
			total += len([]rune(s))
		}
		return total
	}

	emit := func(body string, start, end int, headings []string) {
		if strings.TrimSpace(body) == "" {
			return
		}
		// US4 / FR-015: the heading path is spliced INTO the chunk text, not
		// merely recorded beside it. Only the text is embedded, so a
		// section title kept solely in a metadata column contributes nothing
		// to retrieval — which is the entire reason to detect headings.
		// FR-015a's precedence (body first, breadcrumb shortens or is
		// dropped) is buildBreadcrumbContent's existing contract, shared
		// verbatim with the Markdown path.
		piece := chunkPiece{Content: buildBreadcrumbContent(headings, body, size)}
		if start > 0 && end > 0 {
			s, e := start, end
			piece.PageNumber, piece.PageEnd = &s, &e
		}
		if len(headings) > 0 {
			title := headings[len(headings)-1]
			piece.SectionTitle = &title
		}
		pieces = append(pieces, piece)
	}

	flush := func(carryOverlap bool) {
		if len(acc) == 0 {
			if !carryOverlap {
				pendingOverlap, carryStart, carryEnd = "", 0, 0
			}
			return
		}
		body := strings.Join(acc, "\n\n")
		emit(body, accStart, accEnd, accHeadings)
		if carryOverlap && overlap > 0 {
			pendingOverlap = tailRunes(body, overlap)
			// The tail of this piece is the seed for the next one, so the
			// next piece inherits this piece's pages. Attributing the seed
			// to the LAST page of this piece (rather than its whole span)
			// would be more precise but not provably so — the tail may
			// itself straddle the merge seam.
			carryStart, carryEnd = accStart, accEnd
		} else {
			pendingOverlap, carryStart, carryEnd = "", 0, 0
		}
		acc, accStart, accEnd, accHeadings = nil, 0, 0, nil
	}

	// prevEnd is the last page touched by the previous unit, used to spot a
	// page boundary the merge test declined to join.
	prevEnd := 0

	for _, unit := range units {
		// A page boundary that did NOT merge is still a boundary: the
		// paragraph really did end there. Flushing keeps the pre-006
		// behaviour for every such seam — one page's text never packs
		// together with the next page's into a single chunk, and page
		// intervals stay tight instead of creeping across the whole
		// document. Only text the merge test actually joined travels
		// across a page break, which is also where FR-002's "overlap can
		// cross a page boundary" is satisfied: inside a merged paragraph
		// that is long enough to need splitting, the overlap seed crosses
		// the seam like any other. Carrying overlap across a CLEAN break
		// as well was considered and dropped — it would widen every
		// chunk's interval by one page in exchange for context the
		// neighbour window already provides.
		if prevEnd != 0 && unit.PageStart != prevEnd {
			flush(false)
		}
		if len(acc) > 0 && !sameHeadings(accHeadings, unit.Headings) {
			// A section boundary. Flushing here is what keeps a chunk's
			// breadcrumb true of all of its text.
			flush(true)
		}
		prevEnd = unit.PageEnd

		unitLen := len([]rune(unit.Content))
		if unitLen > size {
			flush(false)
			// bodyBudget mirrors chunkMarkdown: leave the breadcrumb room
			// up front so the split bodies plus their prefix still fit,
			// with a floor so a very long breadcrumb cannot starve the body
			// down to nothing.
			budget := size
			if bc := len([]rune(strings.Join(unit.Headings, " > "))); bc > 0 {
				if budget-bc-2 > size/2 {
					budget = budget - bc - 2
				} else if size/2 > 1 {
					budget = size / 2
				}
			}
			for _, body := range chunkBySentence(unit.Content, budget, overlap) {
				emit(body, unit.PageStart, unit.PageEnd, unit.Headings)
			}
			continue
		}

		if len(acc) > 0 && accRuneLen()+2+unitLen > size {
			flush(true)
		}

		next := unit.Content
		if len(acc) == 0 && pendingOverlap != "" {
			next = prependOverlap(pendingOverlap, "\n", unit.Content, size)
			cover(carryStart, carryEnd)
			pendingOverlap, carryStart, carryEnd = "", 0, 0
		}
		cover(unit.PageStart, unit.PageEnd)
		if len(acc) == 0 {
			accHeadings = unit.Headings
		}
		acc = append(acc, next)
	}
	flush(false)
	return pieces
}

// batchStrings splits items into groups of at most size, preserving
// order — ProcessDocument uses it to keep each provider.Client.Embed call
// bounded by embedBatchSize regardless of how many pieces chunking
// produced (up to maxChunksPerDocument).
func batchStrings(items []string, size int) [][]string {
	if size <= 0 || len(items) == 0 {
		return nil
	}
	batches := make([][]string, 0, (len(items)+size-1)/size)
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		batches = append(batches, items[start:end])
	}
	return batches
}

// sameHeadings reports whether two heading stacks are identical.
func sameHeadings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
