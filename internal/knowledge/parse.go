package knowledge

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"

	"rsc.io/pdf"
)

// parsedContent is what parseFile hands to chunkDocument. Only one of the
// two fields is meaningful per file type: txt/md set Text (the whole file,
// verbatim); pdf sets Pages (one entry per page that actually resolved,
// see extractPDFPages) and leaves Text empty — PDF chunking always works
// page-by-page (chunkPDFPages), it never needs the whole-document text as
// one blob.
type parsedContent struct {
	Text  string
	Pages []pdfPage
	// UnreadablePages are the pages the parser could not read at all and
	// skipped (008-unparseable-page-notice). They are NOT in Pages, which
	// is precisely why they need carrying separately: anything computed
	// from Pages — including "which pages have no text layer" — cannot see
	// them, so without this field the fact that they existed is lost.
	UnreadablePages []int
}

// pdfPage is one page's reconstructed plain text plus its 1-indexed
// position in the document — the only page-number signal parseFile can
// honestly offer (see extractPDFPages).
//
// 006-pdf-layout-chunking added Lines. Text is now derived from them
// (strings.Join(lines, "\n")) rather than being the primary output: page
// text as one flat string throws away the typographic signals — font size,
// vertical position, rendered width — that layout.go needs to tell a
// running header from a body line, or a mid-sentence page break from a
// paragraph that simply ended. Text is kept because everything downstream
// of chunking still works on strings, and keeping it makes this change
// provably behaviour-preserving for pages that were single-line anyway.
type pdfPage struct {
	Number int
	Text   string
	Lines  []pdfLine
}

// pdfLine is one reconstructed line of a PDF page plus the typographic
// features layout.go's heuristics run on. It is the smallest unit this
// package carries page coordinates for.
//
// ⚠️ The line segmentation itself is a heuristic and stays one: a new line
// starts when Y drops by more than lineBreakYGap. That threshold is a magic
// number inherited from the original extractor and 006 deliberately did NOT
// fix it (out of scope) — which means a document with unusually large type
// or unusually tight leading gets its lines split wrongly, and every
// judgement layout.go makes on top of these lines inherits that error.
// Anything reported about noise detection or heading detection has to be
// read with that caveat attached.
type pdfLine struct {
	// Text is the line's reconstructed content, spaces included (see
	// extractPDFPages on why spaces must be rebuilt from X gaps).
	Text string
	// Page is the 1-indexed page this line sits on.
	Page int
	// FontSize is the modal font size of the line's glyphs.
	//
	// ⚠️ 0 means UNKNOWN, not "very small". Every judgement that depends on
	// font size must treat 0 as "signal absent" and return false, never
	// compare it numerically — guessing here is how a body line gets called
	// a heading (FR-016: if it can't be recognised, leave it out; don't
	// invent it).
	FontSize float64
	// Y is the line's baseline. PDF's Y axis grows upward, so a LARGER Y is
	// closer to the top of the page — the comparison reads backwards from
	// screen coordinates and is an easy place to introduce an inverted
	// header/footer test.
	Y float64
	// Width is the rendered width of the line: the right edge of its
	// right-most glyph minus the left edge of its left-most one. This is
	// what makes "the previous page's last line was cut off mid-line by the
	// page break" distinguishable from "the paragraph just ended there".
	Width float64
}

// parseFile extracts plain text from a stored file — txt/md are read
// verbatim, pdf goes through extractPDFPages.
func parseFile(storagePath, fileType string) (parsedContent, error) {
	switch fileType {
	case FileTypeTxt, FileTypeMD:
		data, err := os.ReadFile(storagePath)
		if err != nil {
			return parsedContent{}, fmt.Errorf("knowledge: read file: %w", err)
		}
		return parsedContent{Text: string(data)}, nil
	case FileTypePDF:
		pages, unreadable, err := extractPDFPages(storagePath)
		if err != nil {
			return parsedContent{}, err
		}
		return parsedContent{Pages: pages, UnreadablePages: unreadable}, nil
	default:
		return parsedContent{}, fmt.Errorf("knowledge: unsupported file type %q", fileType)
	}
}

// extractPDFPages reconstructs reading-order plain text per page from
// rsc.io/pdf's positioned text fragments. PDF has no inherent
// paragraph/line structure in its content stream — just characters placed
// at (X, Y) coordinates — so this sorts each page's fragments by Y
// descending (PDF Y grows bottom-to-top, so descending Y is top-to-bottom
// reading order) then X ascending, inserting a newline whenever Y drops
// enough to plausibly be a new line. This is a heuristic, not a real
// layout engine — good enough for chunking, not for preserving exact
// document formatting.
//
// The page number returned for each entry is the page's 1-indexed
// position in the PDF's own page tree (doc.Page(pageNum)), which
// rsc.io/pdf resolves independently of how much (or how little) text a
// page contains — this is the one signal parseFile can stand behind
// without guessing, which is why chunkPDFPages can safely tag every chunk
// from a page with it. A page rsc.io/pdf can't resolve at all (page.V.IsNull,
// a malformed page tree entry) is skipped outright rather than emitting an
// empty placeholder that would misrepresent the document's structure. A
// page that resolves but has no extractable text (e.g. a scanned image
// with no text layer) is still returned here with an empty Text — it's
// chunkPDFPages, not this function, that decides to skip emitting chunks
// for it (see its doc comment); if every page comes back empty the
// end-to-end effect is ProcessDocument failing with ErrEmptyContent, which
// is the correct, honest outcome for a scanned PDF. OCR is out of scope.
func extractPDFPages(storagePath string) ([]pdfPage, []int, error) {
	f, err := os.Open(storagePath)
	if err != nil {
		return nil, nil, fmt.Errorf("knowledge: open pdf: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("knowledge: stat pdf: %w", err)
	}

	doc, err := safeNewPDFReader(f, info.Size())
	if err != nil {
		return nil, nil, err
	}

	pages := make([]pdfPage, 0, doc.NumPage())
	var unreadable []int
	for pageNum := 1; pageNum <= doc.NumPage(); pageNum++ {
		texts, status := safePageText(doc, pageNum)
		switch status {
		case pageParsePanicked:
			// This page made rsc.io/pdf panic. Skip it and keep going:
			// one unreadable page should not cost the reader the rest of
			// the document, and it is the same shape as the partially
			// scanned case — take what can honestly be taken.
			unreadable = append(unreadable, pageNum)
			continue
		case pageUnresolvable:
			// A malformed page-tree entry. Skipped outright rather than
			// emitting a placeholder that would misrepresent the document's
			// structure — the pre-006 behaviour, unchanged.
			continue
		}
		// ⚠️ A page that RESOLVED but has no text still gets appended below,
		// with empty Text. That is load-bearing: FR-017 tells a scan apart
		// from an empty file by counting pages that carry text against the
		// document's page count, and a page dropped here would be invisible
		// to that count — every scanned PDF would come back as "empty file"
		// again, which is the exact confusion US5 exists to remove.
		sort.SliceStable(texts, func(i, j int) bool {
			if texts[i].Y != texts[j].Y {
				return texts[i].Y > texts[j].Y
			}
			if texts[i].X != texts[j].X {
				return texts[i].X < texts[j].X
			}
			// Final tie-break so two glyphs stamped at the exact same
			// coordinate can't reorder between runs. SliceStable already
			// preserves input order, but input order comes out of the
			// PDF's content stream — this makes the ordering a property
			// of the values, not of the parser (FR-021/SC-004).
			return texts[i].S < texts[j].S
		})

		// rsc.io/pdf emits one Text fragment per glyph and silently drops
		// space characters entirely (they only advance the cursor, see
		// its showText) — so word boundaries have to be reconstructed
		// from the gap between one glyph's right edge (X+W) and the
		// next's X, not from any space glyph that might "naturally" sit
		// between fragments. Without this, every single character comes
		// out separated by a space ("H i f y" instead of "Hify").
		//
		// Glyphs now accumulate into a per-line buffer instead of one
		// page-wide builder; joining the finished lines with "\n"
		// reproduces the previous output exactly, since the old code
		// wrote that same "\n" at exactly these boundaries.
		lines := make([]pdfLine, 0, 16)
		var cur lineAccumulator
		var lastY, lastRight float64
		for i, t := range texts {
			switch {
			case i == 0:
				cur.start(t)
			case lastY-t.Y > lineBreakYGap:
				lines = appendLine(lines, cur, pageNum)
				cur.start(t)
			case t.X-lastRight > wordGapX:
				cur.writeString(" ")
			}
			cur.add(t)
			lastY = t.Y
			lastRight = t.X + t.W
		}
		lines = appendLine(lines, cur, pageNum)

		texts_ := make([]string, len(lines))
		for i, l := range lines {
			texts_[i] = l.Text
		}
		pages = append(pages, pdfPage{
			Number: pageNum,
			Text:   strings.Join(texts_, "\n"),
			Lines:  lines,
		})
	}

	// Not one page survived, and at least one died panicking: the file is
	// unreadable, not empty and not a scan. Saying so is the whole point —
	// see ErrPDFUnreadable.
	if len(pages) == 0 && len(unreadable) > 0 {
		return nil, nil, ErrPDFUnreadable
	}
	if len(unreadable) > 0 {
		// 008: this list used to end here, in a developer-facing log line,
		// while the uploader saw a "ready" document with no hint that a page
		// was missing. It now travels out to become something they can see.
		slog.Warn("knowledge: some pdf pages could not be parsed and were skipped",
			"unreadable_pages", unreadable,
			"readable_pages", len(pages))
	}
	return pages, unreadable, nil
}

// safeNewPDFReader / safePageText contain rsc.io/pdf's panics.
//
// ⚠️ Why a recover, when this package otherwise treats panic as reserved
// for unrecoverable programmer error (internal/CLAUDE.md): the panic is not
// ours and not a bug in our logic — rsc.io/pdf reports malformed or
// unsupported PDF structures by panicking, which for a library parsing
// untrusted user uploads is an interface we have to adapt to, not a
// contract violation to let through. Two ordinary arXiv papers panic here.
//
// The recover is deliberately narrow: exactly the two rsc.io/pdf calls that
// walk the file's own structure. It must NOT be widened to cover our own
// code — a nil dereference in the line reconstruction below is a real bug
// and should keep crashing loudly in tests rather than be laundered into
// "this PDF is unreadable".
func safeNewPDFReader(f io.ReaderAt, size int64) (doc *pdf.Reader, err error) {
	defer func() {
		if r := recover(); r != nil {
			// The panic value goes to the wrapped error (developer-facing,
			// English, lowercase) — never to the user-facing Message.
			err = fmt.Errorf("knowledge: parse pdf: %v: %w", r, ErrPDFUnreadable)
		}
	}()
	doc, err = pdf.NewReader(f, size)
	if err != nil {
		return nil, fmt.Errorf("knowledge: parse pdf: %w", err)
	}
	return doc, nil
}

// pageParseStatus distinguishes the three outcomes of reading one page,
// which must NOT be collapsed: a page that panicked, a page that does not
// resolve at all, and a page that resolved but happens to carry no text.
// The last one is a scanned page and has to survive into the page list —
// see the comment at the call site.
type pageParseStatus int

const (
	pageParsed pageParseStatus = iota
	pageUnresolvable
	pageParsePanicked
)

// safePageText returns one page's text fragments together with which of the
// three outcomes occurred.
func safePageText(doc *pdf.Reader, pageNum int) (texts []pdf.Text, status pageParseStatus) {
	defer func() {
		if r := recover(); r != nil {
			texts, status = nil, pageParsePanicked
		}
	}()
	page := doc.Page(pageNum)
	if page.V.IsNull() {
		return nil, pageUnresolvable
	}
	return page.Content().Text, pageParsed
}

const (
	// lineBreakYGap / wordGapX are the two magic numbers the original
	// extractor used inline. They are named here rather than fixed: 006
	// explicitly does not re-tune them (out of scope), but it does make
	// them findable, because line segmentation quality now propagates into
	// every layout judgement downstream.
	lineBreakYGap = 2.0
	wordGapX      = 1.0
)

// lineAccumulator collects the glyphs of one line and the geometry
// derived from them. Split out of the loop above only because "which
// glyph starts a line" and "what that line's width is" are two separate
// concerns that were previously tangled in one builder.
type lineAccumulator struct {
	sb       strings.Builder
	sizes    []float64
	y        float64
	minX     float64
	maxRight float64
	started  bool
}

func (a *lineAccumulator) start(t pdf.Text) {
	a.sb.Reset()
	a.sizes = a.sizes[:0]
	a.y = t.Y
	a.minX = t.X
	a.maxRight = t.X + t.W
	a.started = true
}

func (a *lineAccumulator) writeString(s string) { a.sb.WriteString(s) }

func (a *lineAccumulator) add(t pdf.Text) {
	a.sb.WriteString(t.S)
	if t.FontSize > 0 {
		a.sizes = append(a.sizes, t.FontSize)
	}
	if t.X < a.minX {
		a.minX = t.X
	}
	if r := t.X + t.W; r > a.maxRight {
		a.maxRight = r
	}
}

// appendLine finalises the accumulator into a pdfLine, dropping lines that
// are empty or whitespace-only (invariant L1: a blank line is not a line).
func appendLine(lines []pdfLine, a lineAccumulator, pageNum int) []pdfLine {
	if !a.started {
		return lines
	}
	text := a.sb.String()
	if strings.TrimSpace(text) == "" {
		return lines
	}
	width := a.maxRight - a.minX
	if width < 0 {
		width = 0
	}
	return append(lines, pdfLine{
		Text:     text,
		Page:     pageNum,
		FontSize: modeFontSize(a.sizes),
		Y:        a.y,
		Width:    width,
	})
}

// modeFontSize returns the most common font size among a line's glyphs, or
// 0 when there is none to report (see pdfLine.FontSize: 0 means unknown).
//
// ⭐ Determinism: counting into a map and ranging over it to find the
// maximum would make the answer depend on Go's randomised map iteration
// order — the same PDF would then chunk differently on different runs,
// violating SC-004, and it is the kind of bug that reproduces once in
// dozens of runs. So the sizes are sorted first and scanned in order, and
// ties go to the LARGER size (deliberate: on a line that mixes sizes, the
// larger glyphs are the ones a reader would call the line's size).
func modeFontSize(sizes []float64) float64 {
	if len(sizes) == 0 {
		return 0
	}
	sorted := make([]float64, len(sizes))
	copy(sorted, sizes)
	sort.Float64s(sorted)

	best, bestCount := 0.0, 0
	cur, curCount := sorted[0], 0
	for _, s := range sorted {
		if s != cur {
			cur, curCount = s, 0
		}
		curCount++
		// >= rather than > so a later (larger, because sorted ascending)
		// size wins a tie.
		if curCount >= bestCount {
			best, bestCount = cur, curCount
		}
	}
	return best
}
