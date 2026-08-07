package knowledge

import (
	"fmt"
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
}

// pdfPage is one page's reconstructed plain text plus its 1-indexed
// position in the document — the only page-number signal parseFile can
// honestly offer (see extractPDFPages).
type pdfPage struct {
	Number int
	Text   string
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
		pages, err := extractPDFPages(storagePath)
		if err != nil {
			return parsedContent{}, err
		}
		return parsedContent{Pages: pages}, nil
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
func extractPDFPages(storagePath string) ([]pdfPage, error) {
	f, err := os.Open(storagePath)
	if err != nil {
		return nil, fmt.Errorf("knowledge: open pdf: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("knowledge: stat pdf: %w", err)
	}

	doc, err := pdf.NewReader(f, info.Size())
	if err != nil {
		return nil, fmt.Errorf("knowledge: parse pdf: %w", err)
	}

	pages := make([]pdfPage, 0, doc.NumPage())
	for pageNum := 1; pageNum <= doc.NumPage(); pageNum++ {
		page := doc.Page(pageNum)
		if page.V.IsNull() {
			continue
		}
		texts := page.Content().Text
		sort.SliceStable(texts, func(i, j int) bool {
			if texts[i].Y != texts[j].Y {
				return texts[i].Y > texts[j].Y
			}
			return texts[i].X < texts[j].X
		})

		// rsc.io/pdf emits one Text fragment per glyph and silently drops
		// space characters entirely (they only advance the cursor, see
		// its showText) — so word boundaries have to be reconstructed
		// from the gap between one glyph's right edge (X+W) and the
		// next's X, not from any space glyph that might "naturally" sit
		// between fragments. Without this, every single character comes
		// out separated by a space ("H i f y" instead of "Hify").
		var sb strings.Builder
		var lastY, lastRight float64
		for i, t := range texts {
			switch {
			case i == 0:
			case lastY-t.Y > 2:
				sb.WriteString("\n")
			case t.X-lastRight > 1:
				sb.WriteString(" ")
			}
			sb.WriteString(t.S)
			lastY = t.Y
			lastRight = t.X + t.W
		}
		pages = append(pages, pdfPage{Number: pageNum, Text: sb.String()})
	}
	return pages, nil
}
