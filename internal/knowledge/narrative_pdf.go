package knowledge

import (
	"strings"
)

// Page=0 denotes an inserted paragraph/page separator, never evidence.
type pdfNarrativePart struct {
	Text string
	Page int
}

// The canonical PDF source is the cleaned, reassembled paragraph stream. Its
// offsets refer to extracted text, not PDF pixels. Sources are propagated at
// the page merge itself, so identical paragraphs on different pages stay distinct.
func narrativePDFSource(pages []pdfPage) (string, []int) {
	var text strings.Builder
	var pageAt []int
	appendPart := func(p pdfNarrativePart) {
		s := strings.ReplaceAll(p.Text, "\r\n", "\n")
		text.WriteString(s)
		for range s {
			pageAt = append(pageAt, p.Page)
		}
	}
	for i, u := range buildParagraphStreamWithSources(pages, true) {
		if i > 0 {
			appendPart(pdfNarrativePart{Text: "\n\n"})
		}
		for _, p := range u.NarrativeParts {
			appendPart(p)
		}
	}
	return text.String(), pageAt
}
func chunkNarrativePDF(pages []pdfPage, size, overlap int) []chunkPiece {
	text, pageAt := narrativePDFSource(pages)
	pieces := chunkNarrativeWithOptions(text, size, overlap, false, false)
	for i := range pieces {
		p := &pieces[i]
		var segments []narrativeSegment
		first, last := 0, 0
		for _, s := range p.Narrative.Segments {
			if s.DocumentStart == nil {
				segments = append(segments, s)
				continue
			}
			for from := *s.DocumentStart; from < *s.DocumentEnd; {
				page := pageAt[from]
				to := from + 1
				for to < *s.DocumentEnd && pageAt[to] == page {
					to++
				}
				seg := narrativeSegment{ChunkStart: s.ChunkStart + from - *s.DocumentStart, ChunkEnd: s.ChunkStart + to - *s.DocumentStart, IsOverlapCopy: s.IsOverlapCopy}
				if page == 0 {
					seg.IsGeneratedSeparator = true
				} else {
					a, b, pg := from, to, page
					seg.DocumentStart = &a
					seg.DocumentEnd = &b
					seg.Page = &pg
					if first == 0 || page < first {
						first = page
					}
					if page > last {
						last = page
					}
				}
				segments = append(segments, seg)
				from = to
			}
		}
		p.Narrative.Segments = segments
		if first > 0 {
			p.PageNumber = &first
			p.PageEnd = &last
		}
	}
	return pieces
}
