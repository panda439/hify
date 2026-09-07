package knowledge

import "strings"

// Fence state protects headings, dividers and blank lines inside code blocks.
// Closing fences must match the marker and be at least as long as the opener.
type narrativeFence struct {
	marker byte
	count  int
}

func (f *narrativeFence) consume(line string) bool {
	t := strings.TrimSpace(line)
	if f.count > 0 {
		n := 0
		for n < len(t) && t[n] == f.marker {
			n++
		}
		if n >= f.count && strings.TrimSpace(t[n:]) == "" {
			f.count = 0
		}
		return true
	}
	if len(t) < 3 || (t[0] != '`' && t[0] != '~') {
		return false
	}
	n := 0
	for n < len(t) && t[n] == t[0] {
		n++
	}
	if n < 3 || (t[0] == '`' && strings.Contains(t[n:], "`")) {
		return false
	}
	f.marker = t[0]
	f.count = n
	return true
}
func unwrapNarrativeHeading(line string) string {
	t := strings.TrimSpace(line)
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n >= 1 && n <= 6 && n < len(t) && (t[n] == ' ' || t[n] == '\t') {
		t = strings.TrimSpace(t[n:])
		// Optional closing ATX marker must be whitespace-separated.
		end := len(t)
		for end > 0 && t[end-1] == '#' {
			end--
		}
		if end < len(t) && end > 0 && (t[end-1] == ' ' || t[end-1] == '\t') {
			t = strings.TrimSpace(t[:end])
		}
	}
	return t
}
