package knowledge

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// layout.go holds every layout heuristic 006-pdf-layout-chunking added:
// deciding whether a paragraph continues across a page break, spotting
// running headers/footers, and (US4) spotting headings.
//
// Two rules govern everything in this file.
//
// 1. Every judgement here is a PURE FUNCTION over []pdfLine or smaller
// value types. It touches no database, no rsc.io/pdf, no clock. That is
// not tidiness — it is the only shape in which "body text is never
// deleted" (SC-005) can be re-verified cheaply and exhaustively, and the
// only way the determinism requirement (SC-004) can be tested by calling
// the same function twenty times rather than by ingesting a document
// twenty times.
//
// 2. ⭐ NOTHING here may iterate a map to reach a decision or to order
// output. Both statistics this file needs — the modal body line width and
// the cross-page repetition rate — want a map, and Go randomises map
// iteration order. Ranging over one to find a maximum makes the same PDF
// chunk differently between runs, which violates SC-004 and reproduces
// perhaps once in dozens of attempts. Every map here is therefore drained
// into a slice, sorted, and only then scanned.
//
// The two directions of bias are OPPOSITE and both deliberate:
//
//   - Merging across pages leans towards MERGING. The costs are not
//     symmetric: a wrong merge puts two topics in one chunk, which
//     admission and reranking downstream can still cope with; a wrong split
//     cuts a sentence in half, and nothing downstream can put it back —
//     neither half retrieves, and the half that does surface invites the
//     model to finish the sentence itself.
//   - Stripping noise leans towards KEEPING. SC-005 sets the false-deletion
//     rate at 0: a missed header is a little noise in one chunk, a deleted
//     body line is content the user can never retrieve again.

const (
	// fullLineWidthRatio is criterion 3 of the cross-page merge test: the
	// previous page's last line counts as "cut off by the page break"
	// only if it reaches this fraction of that page's modal body width.
	// A line that stops well short of the margin ended because the
	// paragraph ended, not because the page did.
	//
	// 0.85 leaves room for the ragged right edge of ordinary justified-ish
	// text (a long final word that would not fit gets wrapped, leaving a
	// visible gap) while still excluding the obviously short last line of a
	// paragraph. Raising it makes the merge rarer — which, per the bias
	// above, is the WRONG direction to guess in; lower it before raising it.
	fullLineWidthRatio = 0.85

	// headingFontSizeRatio is how much larger than the body's modal size a
	// line must be before its size counts as a heading signal. 1.15 is
	// about the smallest step a reader perceives as "bigger" (12pt body vs
	// 14pt heading is 1.17); below that, size differences are more likely
	// to come from the extractor's per-glyph rounding than from the design
	// of the document.
	headingFontSizeRatio = 1.15

	// widthBucketPt is the bucket size used when taking the mode of line
	// widths. Widths are floats that never repeat exactly, so a mode is
	// only meaningful over buckets; 1pt is fine enough to separate a short
	// header from a full body line and coarse enough that ordinary body
	// lines land in the same bucket.
	widthBucketPt = 1.0
)

// sentenceTerminators are the characters that mean "this line finished a
// sentence" for criterion 1 of the merge test. Both half- and full-width
// forms are listed because a Chinese document mixes them freely, and the
// closing quotes/brackets are here because a line can legitimately end
// "……结束。」" with the terminator one rune in from the end.
const sentenceTerminators = `。．！？；：.!?;:”"』」）)]】`

var (
	// listItemPattern / chapterPattern are criterion 2: a numbered or
	// bulleted item, or a chapter/section heading, is a structural boundary.
	// Whatever follows it on the next page starts something new, even
	// though the line itself carries no terminating punctuation — which is
	// exactly why criterion 1 alone is not enough.
	listItemPattern = regexp.MustCompile(`^\s*(\d+[.、)）]|[-*·•])\s`)
	chapterPattern  = regexp.MustCompile(`^\s*第.{1,3}[章节条部分]`)

	// headingNumberPattern is US4's pattern signal: "1.", "1.1", "2.3.4"
	// at the start of a line. Deliberately anchored and deliberately
	// requiring the trailing separator, so a line opening with a bare year
	// or quantity is not mistaken for a heading.
	headingNumberPattern = regexp.MustCompile(`^\s*\d+(\.\d+)*[.、）)]?\s+\S`)
)

// paragraphUnit is the real input to PDF chunking, replacing "one string
// per page". It is a run of text that reads as continuous, together with
// the closed page interval it covers.
//
// Invariants (data-model.md P1-P5):
//
//	P1  strings.TrimSpace(Content) != ""
//	P2  1 <= PageStart <= PageEnd <= the document's page count
//	P3  a unit that did not cross a page break has PageStart == PageEnd
//	P4  units come out ordered by PageStart, reading order preserved within
//	    a page
//	P5  merging only ever happens at a page boundary — between the last
//	    unit of one page and the first unit of the next
type paragraphUnit struct {
	Content   string
	PageStart int
	PageEnd   int
	// Headings is the heading stack in force where this unit sits, outermost
	// first — the PDF counterpart of chunkMarkdown's heading stack (US4).
	// Empty whenever no heading could be recognised reliably, which is every
	// PDF ever ingested before 006 and remains the honest default (FR-016).
	Headings []string
}

// buildParagraphStream rebuilds the document's paragraph flow from the
// extracted lines, across page boundaries.
//
// ⭐ Why this works on LINES and not on page text. A PDF's reconstructed
// page text contains no blank lines, so splitParagraphs — which splits on
// blank lines — returns the entire page as a single "paragraph". Before
// 006 that was harmless: the page was the unit anyway. It is not harmless
// now. A merged unit would be two whole pages of text, which almost always
// exceeds chunk_size, and every chunk the oversize fallback then produced
// would inherit the full two-page interval — so a chunk whose text sits
// entirely on page 3 would be cited as "pages 3-4". That is precisely the
// dishonest citation FR-011 forbids, arrived at from the other direction.
//
// So a unit here ends where a SENTENCE ends, using the same test the page
// seam uses: a line whose last character terminates a sentence closes the
// unit; a structural line (list item, heading) closes it too, because what
// follows starts something new. Mid-paragraph lines wrap mid-sentence and
// therefore keep the unit open. The result is units of roughly paragraph
// size, each living on exactly one page unless the seam test joined it to
// the next — which is what makes page intervals tight instead of nominal.
//
// Pages with no extractable text contribute nothing and, importantly, do
// NOT join their neighbours: a scanned page sitting between two text pages
// means the text on either side of it is genuinely discontinuous, and
// pretending otherwise would splice together two passages that never
// touched. See mergeAcrossPageBreak's adjacency check.
func buildParagraphStream(pages []pdfPage) []paragraphUnit {
	var stream []paragraphUnit
	// The heading stack runs across the whole document, not per page — a
	// section started on page 3 is still in force on page 4.
	var headings []string
	for _, page := range pages {
		pageUnits, next := unitsOfPage(page, headings)
		headings = next
		if len(pageUnits) == 0 {
			continue
		}
		if n := len(stream); n > 0 && mergeAcrossPageBreak(stream[n-1], pages, page) {
			stream[n-1].Content += "\n" + pageUnits[0].Content
			stream[n-1].PageEnd = page.Number
			pageUnits = pageUnits[1:]
		}
		stream = append(stream, pageUnits...)
	}
	return stream
}

// unitsOfPage groups one page's lines into paragraph-sized units. Every
// unit it returns lies on a single page (P3); only the seam merge in
// buildParagraphStream can widen one.
func unitsOfPage(page pdfPage, headings []string) ([]paragraphUnit, []string) {
	var units []paragraphUnit

	// A page carrying text but no line geometry — a pdfPage built by hand
	// (tests, callers that never went through extractPDFPages) — falls back
	// to the pre-006 behaviour: split on blank lines and treat the page as
	// the unit. Such a page also never merges with its neighbours, because
	// mergeAcrossPageBreak has no lines to evaluate criteria 2 and 3 on.
	// That is the right default: no geometry means no evidence, and the
	// merge is supposed to require evidence, not the absence of it.
	if len(page.Lines) == 0 {
		for _, u := range splitParagraphs(page.Text) {
			units = append(units, paragraphUnit{
				Content: u, PageStart: page.Number, PageEnd: page.Number,
				Headings: append([]string(nil), headings...),
			})
		}
		return units, headings
	}

	var cur []string
	closeUnit := func() {
		if len(cur) == 0 {
			return
		}
		content := strings.Join(cur, "\n")
		cur = nil
		if strings.TrimSpace(content) == "" {
			return
		}
		units = append(units, paragraphUnit{
			Content:   content,
			PageStart: page.Number,
			PageEnd:   page.Number,
			Headings:  append([]string(nil), headings...),
		})
	}
	for _, line := range page.Lines {
		if level := headingLevel(line, page.Lines); level > 0 {
			// A heading closes whatever preceded it and opens a new
			// section. ⚠️ The heading line stays in the body as well as
			// going into the stack. chunkMarkdown removes it, but Markdown
			// KNOWS a heading from its "#" — here it is a guess from font
			// size and shape, and a wrong guess that removed the line would
			// silently delete body text, the one outcome SC-005 rules out.
			// Duplicating a line costs a little redundancy in one chunk's
			// embedding; deleting one costs content the user can never
			// retrieve. The asymmetry decides it.
			closeUnit()
			headings = pushHeading(headings, level, line.Text)
			cur = append(cur, line.Text)
			closeUnit()
			continue
		}
		cur = append(cur, line.Text)
		if endsSentence(line.Text) || isStructuralLine(line, page.Lines) {
			closeUnit()
		}
	}
	closeUnit()
	return units, headings
}

// mergeAcrossPageBreak answers "does prev, which ends on some earlier
// page, continue into next?" for the seam between two pages.
//
// It refuses outright unless the two pages are physically adjacent
// (prev.PageEnd+1 == next.Number). Without that check, a blank or scanned
// page in the middle would let text from page 3 be glued to text from page
// 7 — a fabricated continuity, and one whose page interval would then be a
// lie as well.
func mergeAcrossPageBreak(prev paragraphUnit, pages []pdfPage, next pdfPage) bool {
	if prev.PageEnd+1 != next.Number {
		return false
	}
	prevLines := linesOfPage(pages, prev.PageEnd)
	if len(prevLines) == 0 {
		return false
	}
	return shouldMergeAcrossPage(prevLines[len(prevLines)-1], prevLines)
}

// shouldMergeAcrossPage is criterion 1 ∧ 2 ∧ 3 on the last line of the
// page that is being left. All three must hold; each one alone is a bad
// predictor:
//
//	criterion 1 alone — a heading carries no full stop either, and gluing a
//	  heading to the next page's first paragraph is a real failure mode in
//	  Chinese documents, where headings are rarely punctuated.
//	criterion 2 alone — plenty of ordinary prose lines are neither lists nor
//	  headings and still end their paragraph.
//	criterion 3 alone — a heading or a list item can easily run the full
//	  width of the text column.
//
// The line's own font size participates through criterion 2: a line
// noticeably larger than the page's body text is a heading regardless of
// how it is punctuated. When the size is unknown (FontSize == 0) that half
// of criterion 2 simply does not fire — unknown is never treated as "not a
// heading, therefore merge", and never treated as a size of zero.
func shouldMergeAcrossPage(last pdfLine, pageLines []pdfLine) bool {
	if endsSentence(last.Text) {
		return false
	}
	if isStructuralLine(last, pageLines) {
		return false
	}
	return isNearFullWidth(last, pageLines)
}

// endsSentence reports whether a line's last non-space rune terminates a
// sentence. Trailing closing punctuation is part of the terminator set, so
// a line ending 「……的规定。」 still counts as finished.
func endsSentence(text string) bool {
	trimmed := strings.TrimRightFunc(text, unicode.IsSpace)
	if trimmed == "" {
		return false
	}
	last, _ := utf8.DecodeLastRuneInString(trimmed)
	return strings.ContainsRune(sentenceTerminators, last)
}

// isStructuralLine reports whether a line is a list item or a heading —
// something whose end is a boundary by construction, not by punctuation.
func isStructuralLine(line pdfLine, pageLines []pdfLine) bool {
	if listItemPattern.MatchString(line.Text) || chapterPattern.MatchString(line.Text) {
		return true
	}
	return hasHeadingFontSize(line, pageLines)
}

// hasHeadingFontSize reports the font-size half of the heading signal.
//
// ⚠️ Returns false whenever the size is unknown on either side (0 means
// unknown, never "small" — see pdfLine.FontSize). A missing signal is a
// missing signal; inferring from its absence is exactly the fabrication
// FR-016 rules out.
func hasHeadingFontSize(line pdfLine, pageLines []pdfLine) bool {
	if line.FontSize <= 0 {
		return false
	}
	body := modalBodyFontSize(pageLines)
	if body <= 0 {
		return false
	}
	return line.FontSize >= body*headingFontSizeRatio
}

// isNearFullWidth is criterion 3: the line reaches far enough across the
// text column to read as "wrapped by the page edge" rather than "ended".
func isNearFullWidth(line pdfLine, pageLines []pdfLine) bool {
	mode := modalLineWidth(pageLines)
	if mode <= 0 {
		// No usable width signal at all (a one-line page, or an extractor
		// that reported no geometry). Per the merge bias, absence of a
		// reason not to merge is not a reason to split — but criteria 1
		// and 2 have already had their say, so this is not an unguarded
		// yes.
		return true
	}
	return line.Width >= mode*fullLineWidthRatio
}

// modalLineWidth is the most common line width on a page, bucketed to
// widthBucketPt. This is the page's "body width": headers, footers and
// paragraph-final lines are all shorter, and there are more body lines
// than any of those.
//
// ⭐ Determinism: bucket counts live in a map, so the counts are drained
// into a slice and SORTED before the maximum is taken; ties go to the
// WIDER bucket, because between two equally common widths the wider one is
// the better candidate for "full column width". Reaching the maximum by
// ranging the map directly is the single most dangerous bug available in
// this file — it is invisible in a passing test run and violates SC-004.
func modalLineWidth(lines []pdfLine) float64 {
	counts := make(map[int]int, len(lines))
	for _, l := range lines {
		if l.Width <= 0 {
			continue
		}
		counts[int(math.Round(l.Width/widthBucketPt))]++
	}
	bucket, ok := modalBucket(counts)
	if !ok {
		return 0
	}
	return float64(bucket) * widthBucketPt
}

// modalBodyFontSize is the most common font size on a page — the size a
// heading has to stand out from. Same determinism rules as modalLineWidth;
// ties go to the SMALLER size here, deliberately: the body is the smaller
// text, and resolving a tie upwards would raise the bar a heading has to
// clear and lose real headings.
func modalBodyFontSize(lines []pdfLine) float64 {
	counts := make(map[int]int, len(lines))
	for _, l := range lines {
		if l.FontSize <= 0 {
			continue
		}
		counts[int(math.Round(l.FontSize*10))]++
	}
	best, bestCount, found := 0, 0, false
	for _, k := range sortedKeys(counts) {
		// Strictly greater, so the first (smallest, because sortedKeys is
		// ascending) key wins a tie.
		if counts[k] > bestCount {
			best, bestCount, found = k, counts[k], true
		}
	}
	if !found {
		return 0
	}
	return float64(best) / 10
}

// modalBucket returns the most frequent key, resolving ties towards the
// larger key. Split out so both callers share one audited implementation
// of "drain, sort, then scan".
func modalBucket(counts map[int]int) (int, bool) {
	best, bestCount, found := 0, 0, false
	for _, k := range sortedKeys(counts) {
		// >= so a later (larger, ascending order) key wins a tie.
		if counts[k] >= bestCount {
			best, bestCount, found = k, counts[k], true
		}
	}
	return best, found
}

// sortedKeys is the guard rail behind rule 2 at the top of this file: it
// is the ONLY way this package should ever walk a counting map.
func sortedKeys(counts map[int]int) []int {
	keys := make([]int, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// linesOfPage returns the lines belonging to a 1-indexed page number.
func linesOfPage(pages []pdfPage, number int) []pdfLine {
	for _, p := range pages {
		if p.Number == number {
			return p.Lines
		}
	}
	return nil
}

// --- Layout noise: running headers, running footers, page-number lines ---

const (
	// noiseBandRatio is criterion 1 of the noise test: how much of a page's
	// vertical extent counts as "the top" or "the bottom". 0.15 at each end
	// leaves the middle 70% of the page immune to noise detection outright,
	// which is the cheapest possible guarantee that a paragraph in the body
	// of the page can never be deleted no matter how often it repeats.
	noiseBandRatio = 0.15

	// noiseRepeatPageRatio is criterion 2: the share of pages a normalised
	// line must appear on before repetition counts as evidence of a running
	// header or footer. 0.6 is well above what any real sentence achieves
	// and well below what a header printed on every page achieves, so the
	// gap on either side of it is wide — this threshold is not delicately
	// balanced, and it should not be tuned to rescue a single document.
	noiseRepeatPageRatio = 0.6

	// noiseMaxWidthRatio is criterion 3: a noise line has to be markedly
	// shorter than the body's column width. A running header is a few words;
	// half the column is generous. This is the criterion that saves a short
	// repeated body line — a recurring defined term, a table row — because
	// such a line still has to be BOTH in the margin band AND short.
	noiseMaxWidthRatio = 0.5

	// minPagesForRepeatNoise implements FR-009. Below this, the repetition
	// rate is not a statistic, it is an accident: on a two-page document any
	// line appearing twice is at 100%. The rule is to strip nothing rather
	// than to guess — a missed header costs a little noise, a deleted body
	// line costs content that can never be retrieved again (SC-005).
	minPagesForRepeatNoise = 3

	// noiseLogTextLimit / noiseLogLineLimit bound the audit trail (FR-008).
	// Headers are short by construction, and a thousand-page document must
	// not be able to flood the log; beyond the line limit only the count is
	// carried forward.
	noiseLogTextLimit = 80
	noiseLogLineLimit = 50
)

// pageMarkerPattern matches the UNAMBIGUOUS page-marker forms: "3 / 12",
// "第 3 页", "第 3 页 / 共 12 页", "Page 3 of 12". Anchored at both ends on
// purpose — a line that merely CONTAINS one of these is ordinary text.
// Nothing else in a document looks like these, so they need no further
// corroboration.
var pageMarkerPattern = regexp.MustCompile(
	`^\s*(?:` +
		`\d+\s*[/／-]\s*\d+` +
		`|第\s*\d+\s*页(?:\s*[/／,，]?\s*共\s*\d+\s*页)?` +
		`|[Pp]age\s+\d+(?:\s+of\s+\d+)?` +
		`)\s*$`)

// bareNumberLinePattern matches a line that is ONLY a number.
//
// ⚠️ This form is treated far more suspiciously than the ones above, and
// the reason is a real defect caught by running a real paper through the
// pipeline: an academic PDF is full of one- and two-character lines that
// are nothing but a number — subscripts and superscripts the extractor
// places on their own baseline, footnote markers, equation numbers. Under
// a rule that stripped any bare number near a margin, twenty such lines
// were deleted from a fifteen-page paper. They were body text. SC-005 puts
// the false-deletion rate at 0, so a bare number now has to ALSO be the
// outermost line on its page and hold a value that could actually be a
// page number in this document (see isBareNumberPageMarker).
var bareNumberLinePattern = regexp.MustCompile(`^\s*\d{1,4}\s*$`)

// noiseReason enumerates why a line was stripped, for the audit log.
type noiseReason string

const (
	reasonRepeatedHeader noiseReason = "repeated_header"
	reasonRepeatedFooter noiseReason = "repeated_footer"
	reasonPageNumberLine noiseReason = "page_number_line"
)

// noiseRecord is one stripped line, kept for after-the-fact review of
// whether anything was deleted that should not have been (FR-008).
type noiseRecord struct {
	Page            int
	Reason          noiseReason
	LineLength      int
	RepeatPageRatio float64
	// Text is the NORMALISED form (digits removed), truncated — see
	// service.go's audit logging for why recording it at all is a
	// deliberate departure from this package's usual "never log content"
	// rule, and what bounds that departure.
	Text string
}

// stripLayoutNoise removes running headers, running footers and standalone
// page-number lines, returning the cleaned pages and an audit record of
// everything it took out.
//
// ⚠️ Direction of bias, opposite to the merge test's: WHEN IN DOUBT, KEEP.
// SC-005 fixes the false-deletion rate at 0, and the asymmetry is stark —
// a header that slips through adds a few words of noise to one chunk's
// embedding, while a body line deleted here is gone from the index and the
// user has no way to know it is missing. That is why the three criteria are
// an AND and not an OR: position alone deletes the first line of every
// page, repetition alone deletes a recurring defined term, shortness alone
// deletes every paragraph's last line.
//
// Complexity is O(total lines): one pass to count normalised lines per
// page, one pass to decide. Noise detection needs cross-page statistics,
// but it must not become O(pages²) — a slowdown that grows with page count
// is an algorithm error, not a tuning problem.
func stripLayoutNoise(pages []pdfPage) ([]pdfPage, []noiseRecord) {
	pageCount := len(pages)
	repeat := repeatRatios(pages)

	var records []noiseRecord
	out := make([]pdfPage, 0, len(pages))
	for _, page := range pages {
		if len(page.Lines) == 0 {
			out = append(out, page)
			continue
		}
		topY, bottomY := verticalExtent(page.Lines)
		band := (topY - bottomY) * noiseBandRatio
		bodyWidth := modalLineWidth(page.Lines)

		kept := make([]pdfLine, 0, len(page.Lines))
		for _, line := range page.Lines {
			inTop := line.Y >= topY-band
			inBottom := line.Y <= bottomY+band
			if !inTop && !inBottom {
				// Criterion 1 failed outright. Nothing in the body of a
				// page is ever removed, whatever else is true of it.
				kept = append(kept, line)
				continue
			}

			ratio := repeat[normalizeNoiseText(line.Text)]
			outermost := line.Y == topY || line.Y == bottomY
			if reason, ok := noiseVerdict(line, pageCount, ratio, bodyWidth, inTop, outermost); ok {
				records = append(records, noiseRecord{
					Page:            page.Number,
					Reason:          reason,
					LineLength:      utf8.RuneCountInString(line.Text),
					RepeatPageRatio: ratio,
					Text:            truncateRunes(normalizeNoiseText(line.Text), noiseLogTextLimit),
				})
				continue
			}
			kept = append(kept, line)
		}

		texts := make([]string, len(kept))
		for i, l := range kept {
			texts[i] = l.Text
		}
		out = append(out, pdfPage{
			Number: page.Number,
			Text:   strings.Join(texts, "\n"),
			Lines:  kept,
		})
	}
	return out, records
}

// noiseVerdict decides a single line that has already passed criterion 1
// (it sits in a margin band).
//
// The page-number rule is independent of the repetition statistic: a line
// that is nothing but "第 3 页 / 共 12 页" never repeats verbatim, so no
// repetition threshold could ever catch it, and it is unmistakable on its
// own.
//
// ⚠️ Deliberately STRICTER than quickstart §4.2, which describes the
// page-number rule as not requiring any of the three criteria: here it
// still requires the margin band. Without that, a table cell in the middle
// of a page containing only "3" would be deleted as a page number — body
// text, silently gone, which is the one outcome SC-005 rules out. The
// deviation only ever keeps more content, never less.
func noiseVerdict(line pdfLine, pageCount int, ratio, bodyWidth float64, inTop, outermost bool) (noiseReason, bool) {
	if pageMarkerPattern.MatchString(line.Text) {
		return reasonPageNumberLine, true
	}
	if outermost && isBareNumberPageMarker(line.Text, pageCount) {
		return reasonPageNumberLine, true
	}
	if pageCount < minPagesForRepeatNoise {
		return "", false // FR-009
	}
	if ratio < noiseRepeatPageRatio {
		return "", false // criterion 2
	}
	if bodyWidth <= 0 || line.Width > bodyWidth*noiseMaxWidthRatio {
		return "", false // criterion 3
	}
	if inTop {
		return reasonRepeatedHeader, true
	}
	return reasonRepeatedFooter, true
}

// repeatRatios maps each normalised line text to the share of PAGES it
// appears on. Pages, not occurrences: a line printed three times on one
// page is not a running header, and counting occurrences would let it look
// like one.
func repeatRatios(pages []pdfPage) map[string]float64 {
	if len(pages) == 0 {
		return nil
	}
	pagesWith := make(map[string]int)
	for _, page := range pages {
		seen := make(map[string]bool, len(page.Lines))
		for _, line := range page.Lines {
			key := normalizeNoiseText(line.Text)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			pagesWith[key]++
		}
	}
	ratios := make(map[string]float64, len(pagesWith))
	for k, n := range pagesWith {
		ratios[k] = float64(n) / float64(len(pages))
	}
	// Note: this map is only ever LOOKED UP by key, never iterated to reach
	// a decision or to order anything, so rule 2 at the top of this file is
	// satisfied without sorting.
	return ratios
}

// normalizeNoiseText is the form repetition is counted in: digits removed
// (so "第 3 页" and "第 4 页" are the same header), whitespace collapsed,
// case folded. Everything else is left alone — the goal is to recognise the
// same header across pages, not to compare meaning.
func normalizeNoiseText(text string) string {
	var sb strings.Builder
	lastSpace := false
	for _, r := range text {
		switch {
		case unicode.IsDigit(r):
			// dropped
		case unicode.IsSpace(r):
			if !lastSpace && sb.Len() > 0 {
				sb.WriteRune(' ')
				lastSpace = true
			}
		default:
			sb.WriteRune(unicode.ToLower(r))
			lastSpace = false
		}
	}
	return strings.TrimSpace(sb.String())
}

// verticalExtent returns the highest and lowest baseline on a page. PDF Y
// grows upward, so "top" is the maximum.
func verticalExtent(lines []pdfLine) (top, bottom float64) {
	top, bottom = lines[0].Y, lines[0].Y
	for _, l := range lines[1:] {
		if l.Y > top {
			top = l.Y
		}
		if l.Y < bottom {
			bottom = l.Y
		}
	}
	return top, bottom
}

func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}

// --- Scanned / image-only PDFs (FR-017) ---

// textLayerCoverage reports how many of a PDF's pages carried extractable
// text, and which ones did not.
//
// The ratio is the whole of this feature's scanned-PDF work (FR-019 rules
// out OCR and visual retrieval here), and it exists to turn one confusing
// failure into an actionable message. Before, every page coming back empty
// produced "文档内容为空或无法提取到文本" — accurate, and useless: it gave
// the user no way to tell "I uploaded an empty file" from "I uploaded a
// scan that needs OCR", which are two completely different next steps.
//
// Three bands, per research.md R6:
//
//	withText == 0             → the document has no text layer at all
//	0 < withText < len(pages) → some pages are images; the rest ingests
//	withText == len(pages)    → nothing to report
func textLayerCoverage(pages []pdfPage) (withText int, pagesWithoutText []int) {
	for _, p := range pages {
		if strings.TrimSpace(p.Text) == "" {
			pagesWithoutText = append(pagesWithoutText, p.Number)
			continue
		}
		withText++
	}
	return withText, pagesWithoutText
}

// --- Headings (US4, P3) ---

// headingLevel reports the heading depth of a line, or 0 if it is not a
// heading. US4 is a SHOULD, and this is where that shows: detection uses
// DOUBLE-SIGNAL cross-validation — the line must both be visibly larger
// than the page's body text AND match a heading-shaped pattern.
//
// Either signal alone is a bad detector. Size alone promotes any emphasised
// pull quote or cover text to a heading; pattern alone promotes a numbered
// list item. Requiring both trades recall for precision, which is the
// direction FR-016 asks for: when a heading cannot be recognised reliably,
// leave the section blank rather than invent one.
//
// ⚠️ When the font size is unknown (0), hasHeadingFontSize returns false and
// so does this: no heading. That is the honest degradation — this whole
// capability is allowed to produce nothing, and produced nothing before 006
// for every PDF ever ingested.
func headingLevel(line pdfLine, pageLines []pdfLine) int {
	if !hasHeadingFontSize(line, pageLines) {
		return 0
	}
	text := strings.TrimSpace(line.Text)
	if chapterPattern.MatchString(text) {
		return 1
	}
	if m := headingNumberPattern.FindString(text); m != "" {
		// "1." → level 1, "1.1" → level 2, "2.3.4" → level 3.
		return strings.Count(strings.TrimSpace(m), ".") + 1 - boolToInt(strings.HasSuffix(strings.Fields(m)[0], "."))
	}
	if isShortAllCaps(text) {
		return 1
	}
	return 0
}

// isShortAllCaps recognises the unnumbered heading form: a short line in
// capitals. Length-bounded because a long shouted sentence is not a
// heading, and it requires at least one cased letter so a line of digits
// or punctuation cannot qualify.
func isShortAllCaps(text string) bool {
	if utf8.RuneCountInString(text) > 40 {
		return false
	}
	hasCased := false
	for _, r := range text {
		if unicode.IsLower(r) {
			return false
		}
		if unicode.IsUpper(r) {
			hasCased = true
		}
	}
	return hasCased
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// pushHeading applies a heading to a running stack, replacing everything at
// or below its level — the same shape chunkMarkdown's heading stack has, so
// PDF and Markdown produce the same kind of breadcrumb.
func pushHeading(stack []string, level int, text string) []string {
	if level < 1 {
		return stack
	}
	if level > len(stack)+1 {
		level = len(stack) + 1
	}
	stack = append(stack[:level-1:level-1], strings.TrimSpace(text))
	return stack
}

// isBareNumberPageMarker reports whether a number-only line can plausibly be
// this document's page number: it has to parse, and it has to fall inside
// the document's actual page range. A "2" on page 9 of a 15-page paper is
// far more likely to be a subscript than a page number, but this only rules
// out numbers larger than the document — the cheap, unarguable half. The
// expensive half of the guard is the caller's: the line must also be the
// single outermost line on its page, which a subscript sitting among body
// lines never is.
func isBareNumberPageMarker(text string, pageCount int) bool {
	if !bareNumberLinePattern.MatchString(text) {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return false
	}
	return n >= 1 && n <= pageCount
}
