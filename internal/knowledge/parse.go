package knowledge

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"rsc.io/pdf"
)

// parseFile extracts plain text from a stored file — txt/md are read
// verbatim, pdf goes through extractPDFText.
func parseFile(storagePath, fileType string) (string, error) {
	switch fileType {
	case FileTypeTxt, FileTypeMD:
		data, err := os.ReadFile(storagePath)
		if err != nil {
			return "", fmt.Errorf("knowledge: read file: %w", err)
		}
		return string(data), nil
	case FileTypePDF:
		return extractPDFText(storagePath)
	default:
		return "", fmt.Errorf("knowledge: unsupported file type %q", fileType)
	}
}

// extractPDFText reconstructs reading-order plain text from rsc.io/pdf's
// positioned text fragments. PDF has no inherent paragraph/line structure
// in its content stream — just characters placed at (X, Y) coordinates —
// so this sorts each page's fragments by Y descending (PDF Y grows
// bottom-to-top, so descending Y is top-to-bottom reading order) then X
// ascending, inserting a newline whenever Y drops enough to plausibly be a
// new line. This is a heuristic, not a real layout engine — good enough
// for chunking, not for preserving exact document formatting.
func extractPDFText(storagePath string) (string, error) {
	f, err := os.Open(storagePath)
	if err != nil {
		return "", fmt.Errorf("knowledge: open pdf: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("knowledge: stat pdf: %w", err)
	}

	doc, err := pdf.NewReader(f, info.Size())
	if err != nil {
		return "", fmt.Errorf("knowledge: parse pdf: %w", err)
	}

	var sb strings.Builder
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
		sb.WriteString("\n\n")
	}
	return sb.String(), nil
}
