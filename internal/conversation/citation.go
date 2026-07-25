package conversation

import (
	"bytes"
	"encoding/xml"
	"regexp"
	"strings"
)

// citationRefPattern is the only citation syntax the model is allowed to
// produce — see CLAUDE.md's Citation V1 spec. [1-9][0-9]* rules out [S0]
// and anything with a leading zero; there is no way to express a negative
// or non-numeric ref ([S-1], [source1]) with this pattern, so those simply
// never match and pass through parseCitations untouched (not stripped,
// just never recognized as a citation).
var citationRefPattern = regexp.MustCompile(`\[S([1-9][0-9]*)\]`)

// normalizeCitations is the single pure function both runStream (after
// accumulating a full streamed reply) and its tests use to turn a raw model
// answer into (cleaned content, ordered valid refs, invalid ref count). It
// must run on the complete answer, never on an individual SSE delta — a
// [S1] marker can legitimately span two deltas ("[S" / "1]"), so only the
// fully assembled string is safe to regex against.
//
// allowedRefs is the exact set of refs this turn actually assigned to
// evidence sent to the model (see selectEvidence) — a ref not in this set
// is illegal even if it's well-formed ([S999] when only S1/S2 were ever
// offered), and is deleted from the returned content rather than merely
// left unlinked. Duplicate occurrences of the same valid ref collapse to
// one entry in the returned order slice, keyed by first appearance;
// invalidCount, by contrast, counts every illegal occurrence (not
// deduped) — it backs the trace span's raw "how many times did the model
// try this" signal, not a citation list.
func normalizeCitations(content string, allowedRefs map[string]struct{}) (cleaned string, validRefs []string, invalidCount int) {
	matches := citationRefPattern.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return content, nil, 0
	}

	var out strings.Builder
	seen := make(map[string]bool, len(matches))
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		ref := "S" + content[m[2]:m[3]]
		out.WriteString(content[last:start])
		if _, ok := allowedRefs[ref]; ok {
			out.WriteString(content[start:end])
			if !seen[ref] {
				seen[ref] = true
				validRefs = append(validRefs, ref)
			}
		} else {
			// Invalid ref: dropped entirely, nothing written for this span
			// — per spec, a hallucinated/out-of-range citation must not
			// survive into the saved/returned content.
			invalidCount++
		}
		last = end
	}
	out.WriteString(content[last:])
	return out.String(), validRefs, invalidCount
}

// escapeXMLAttr renders s safe to embed inside a double-quoted XML
// attribute value — used for the ref/document/section attributes of
// <source>, whose document_name/section_title values come from
// user-uploaded file names and are not trusted to be free of `"`, `<`, `&`,
// or even embedded newlines that could otherwise break out of the
// attribute or the <retrieved_sources> boundary itself (see context.go).
// xml.EscapeText escaping newlines as entities is exactly what an
// attribute value needs (a literal newline inside "..." would be
// malformed), unlike escapeXMLBody below.
func escapeXMLAttr(s string) string {
	var buf bytes.Buffer
	// xml.EscapeText never returns an error for a bytes.Buffer target (it
	// only writes to an io.Writer, whose own write can't fail here).
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// xmlBodyReplacer escapes only what would actually break a <source> text
// node's boundary (&, <, >) — order matters, & must go first or the escaped
// entities for < and > would themselves get re-escaped. Deliberately not
// xml.EscapeText: that also turns every newline into an "&#xA;" entity,
// which would mangle the natural paragraph structure of retrieved chunk
// content and make it harder for the model to actually read as reference
// text, for no security benefit (a literal newline can't break out of an
// XML text node the way an unescaped `<` could).
var xmlBodyReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escapeXMLBody(s string) string {
	return xmlBodyReplacer.Replace(s)
}
