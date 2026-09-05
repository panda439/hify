package knowledge

import (
	"database/sql"
	"sort"
	"strconv"
	"strings"
)

// notice.go encodes and decodes the page list behind Document's
// UnextractedPages — the pages a PDF's text extraction could not read
// (007-document-processing-notice).
//
// It lives in its own file for one reason: these two functions are pure,
// they have real edge cases (empty, out of order, duplicated, thousands of
// pages, corrupt historical values), and folding them into repository.go
// would make every one of those cases reachable only through a database.

// unextractedPagesSep is the on-disk separator. The stored form is
// deliberately dumb — "2,4,17" — because nothing ever queries INTO this
// column: it is read out whole and rendered. A structured column type
// would buy expressiveness this feature has no use for, and would invite
// the "just add a code field too" change that FR-011 exists to prevent.
const unextractedPagesSep = ","

// encodeUnextractedPages turns a page list into its stored form.
//
// An empty result is NULL, never the empty string. "This document has no
// missing pages" and "this document has a zero-length list of missing
// pages" are the same fact, and giving one fact two representations means
// every reader downstream has to handle both — or, more likely, handle one
// and be quietly wrong about the other.
//
// ⭐ The sort is not decoration. textLayerCoverage currently happens to
// emit pages in ascending order, but that is an implementation detail of
// the caller: relying on it would make the stored order a property of the
// call chain rather than of the value. Sorting here means the same set of
// pages always stores identically, whatever upstream does later
// (constitution V).
//
// Non-positive page numbers are dropped rather than stored: pages are
// 1-indexed everywhere in this package, so a 0 or a negative is a bug in
// the caller, and persisting it would put a page number in the database
// that cannot refer to any page.
func encodeUnextractedPages(pages []int) sql.NullString {
	if len(pages) == 0 {
		return sql.NullString{}
	}

	seen := make(map[int]struct{}, len(pages))
	cleaned := make([]int, 0, len(pages))
	for _, p := range pages {
		if p < 1 {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		cleaned = append(cleaned, p)
	}
	if len(cleaned) == 0 {
		return sql.NullString{}
	}
	sort.Ints(cleaned)

	parts := make([]string, len(cleaned))
	for i, p := range cleaned {
		parts[i] = strconv.Itoa(p)
	}
	return sql.NullString{String: strings.Join(parts, unextractedPagesSep), Valid: true}
}

// decodeUnextractedPages reads the stored form back.
//
// ⚠️ It never returns an error. A value it cannot parse yields nil — the
// same as "no notice". This is a display-only column: letting a malformed
// historical value fail the decode would take down the entire document
// list, turning a cosmetic data problem into an outage of the page the
// user needs in order to see anything at all. Silently showing no notice
// is the mild failure; that trade is made deliberately here.
//
// Anything it does return satisfies the same invariants encode produces:
// ascending, deduplicated, all >= 1.
func decodeUnextractedPages(raw sql.NullString) []int {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	fields := strings.Split(raw.String, unextractedPagesSep)
	pages := make([]int, 0, len(fields))
	for _, f := range fields {
		p, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || p < 1 {
			continue
		}
		pages = append(pages, p)
	}
	if len(pages) == 0 {
		return nil
	}
	// Re-normalise rather than trust what is on disk: a row written by an
	// older or buggier version is exactly the case this has to survive.
	sort.Ints(pages)
	out := pages[:1]
	for _, p := range pages[1:] {
		if p != out[len(out)-1] {
			out = append(out, p)
		}
	}
	return out
}
