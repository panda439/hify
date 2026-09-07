package knowledge

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// sourceRune follows a character through trimming, joins and overlap. Byte=-1
// denotes an inserted separator. Repeated text is never searched for afterwards.
type sourceRune struct {
	Byte int
	Copy bool
}

func spanOrigins(s textSpan) []sourceRune {
	if s.Origins != nil {
		return s.Origins
	}
	out := make([]sourceRune, 0, utf8.RuneCountInString(s.Text))
	for b := range s.Text {
		out = append(out, sourceRune{Byte: s.Start + b})
	}
	return out
}
func joinedOrigins(spans []textSpan, sep string) []sourceRune {
	var out []sourceRune
	for i, s := range spans {
		if i > 0 {
			for range sep {
				out = append(out, sourceRune{Byte: -1})
			}
		}
		out = append(out, spanOrigins(s)...)
	}
	return out
}
func trimOrigins(body, trimmed string, origins []sourceRune) []sourceRune {
	lead := utf8.RuneCountInString(body) - utf8.RuneCountInString(strings.TrimLeftFunc(body, unicode.IsSpace))
	return origins[lead : lead+utf8.RuneCountInString(trimmed)]
}
func copyOriginsTail(origins []sourceRune, n int) []sourceRune {
	out := append([]sourceRune(nil), origins[len(origins)-n:]...)
	for i := range out {
		out[i].Copy = true
	}
	return out
}
func prefixedOrigins(body textSpan, tail []sourceRune, prefix int) []sourceRune {
	if prefix == 0 {
		return spanOrigins(body)
	}
	out := append([]sourceRune(nil), tail[len(tail)-(prefix-1):]...)
	out = append(out, sourceRune{Byte: -1})
	return append(out, spanOrigins(body)...)
}

// sourceSegments coalesces only adjacent source positions with the same flags.
func sourceSegments(ri *runeIndex, body textSpan) []narrativeSegment {
	var out []narrativeSegment
	for i, o := range spanOrigins(body) {
		generated := o.Byte < 0
		from := 0
		if !generated {
			from = ri.at(o.Byte)
		}
		if len(out) > 0 {
			last := &out[len(out)-1]
			if last.IsGeneratedSeparator == generated && last.IsOverlapCopy == o.Copy && (generated || *last.DocumentEnd == from) {
				last.ChunkEnd++
				if !generated {
					end := from + 1
					last.DocumentEnd = &end
				}
				continue
			}
		}
		seg := narrativeSegment{ChunkStart: i, ChunkEnd: i + 1, IsGeneratedSeparator: generated, IsOverlapCopy: o.Copy}
		if !generated {
			end := from + 1
			seg.DocumentStart = &from
			seg.DocumentEnd = &end
		}
		out = append(out, seg)
	}
	return out
}
