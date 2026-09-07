package knowledge

import (
	"regexp"
	"strconv"
	"strings"
)

// narrative.go splits narrative prose (novels) by SCENE rather than by
// length (010-narrative-scene-chunking-and-relation-extraction, US1).
//
// The existing chunkers assume the semantic unit is something the format
// hands you: markdown gives headings, PDF gives pages. Prose gives neither
// — its unit is the scene, and a scene has no marker other than a chapter
// heading or a divider line. Chopping it at a character count splits a
// scene in half, and each half then lacks the cause or the consequence
// that made it meaningful.
//
// This is the same mistake 006 fixed for PDFs, in a different costume:
// cutting a semantic unit with a physical one. 006 removed "a page is a
// semantic boundary"; this removes "a length is a semantic boundary".
//
// ⚠️ The hard length limit still wins. A scene that cannot fit is split —
// by paragraph, then by sentence, then by character — because the limit
// protects embedding quality and the per-document chunk cap. Scene
// integrity is a preference, not an override (FR-002/FR-003).
//
// ⭐ Everything here is a PURE FUNCTION over strings. No database, no
// model call, no clock. That is what makes US1 independently deliverable
// and independently testable — and it is the half of this feature whose
// output CAN be compared byte for byte (the extraction half cannot).

// sceneDividerPattern matches an explicit scene break: a line consisting
// only of divider punctuation. Novels use these to mark a cut inside a
// chapter — a jump in time or viewpoint.
//
// ⚠️ Deliberately anchored to the WHOLE line. A line that merely contains
// a run of asterisks is prose; a line that is nothing but them is a
// divider. The distinction matters because dialogue-heavy prose is full of
// short lines, and a loose pattern would shred it into fragments.
//
// ⚠️ Interior whitespace is allowed because books write both `***` and
// `* * *`. A first version required the marks to be adjacent and silently
// treated `* * *` as prose — the divider simply did not exist as far as the
// splitter was concerned, and the two scenes it separated were merged into
// one with no error. Neither public-domain corpus on hand contains ANY
// divider line, so nothing but a constructed fixture can catch this.
//
// ⚠️ ※ appears in BOTH branches on purpose. The `※+` branch exists because
// a single ※ on its own line is already a divider (unlike a single `*`,
// which is prose). It must NOT be the only place ※ appears: `※ ※ ※` — the
// most common form in Chinese novels — has interior whitespace and only the
// general branch can match it. A version that moved ※ out of the general
// class to give it a `※+` branch silently stopped recognising the spaced
// form, and the two scenes it separated merged with no error.
//
// ⚠️ 空白用 `[\s\p{Zs}]` 而不是 `\s`：Go 的 RE2 里 `\s` 只有
// `[\t\n\f\r ]`，**不含全角空格 U+3000**，而中文排版里 `※　※　※`
// 用的正是全角空格。只写 `\s` 的话这一行会被当成行文，两个场景合成一个。
var sceneDividerPattern = regexp.MustCompile(
	`^[\s\p{Zs}]*(?:※+|[*※·＊\-—–─＿_=](?:[\s\p{Zs}]*[*※·＊\-—–─＿_=]){2,})[\s\p{Zs}]*$`)

// chapterHeadingPattern matches the common Chinese chapter-heading forms.
//
// ⚠️ The numeral class includes BOTH `〇` (U+3007 IDEOGRAPHIC NUMBER ZERO)
// and `○` (U+25CB WHITE CIRCLE). That is not defensive padding: the
// Project Gutenberg text of 西遊記 uses U+25CB, and a first version of
// this pattern that only knew U+3007 silently missed 10 of its 100
// chapter headings — 90 were found, no error, no warning. Chapter number
// is the time dimension every relation record hangs off (FR-008), so a
// corpus-level miss like that poisons everything downstream while looking
// perfectly healthy.
//
// ⚠️ A separator (whitespace, punctuation, or end of line) is REQUIRED after
// the 章/回 marker, and the whole line is length-capped. Without that, an
// ordinary paragraph opening with `第一章正文……` matches as a heading — and
// the damage is not a wrong title but a wrong SPLIT: the real heading and
// its body become two separate scenes, and every later paragraph that opens
// the same way starts a bogus chapter. A constructed fixture caught this;
// neither corpus happens to contain such a paragraph.
var chapterHeadingPattern = regexp.MustCompile(
	`^\s*第\s*([〇○零两一二三四五六七八九十百千0-9]{1,8})\s*[章回节節卷篇](?:\s+|[　、：:．.·「『(（]|$)\s*(.*)$`)

// maxHeadingRunes bounds how long a line may be and still be a heading.
// 西遊記's are about 20 runes ("第一回　靈根育孕源流出　心性修持大道生");
// prose paragraphs are far longer.
const maxHeadingRunes = 40

// prologueHeadingPattern covers the unnumbered openers (楔子/序章/引子).
// They get no chapter number — inventing one would be a fabricated time
// coordinate (FR-004).
var prologueHeadingPattern = regexp.MustCompile(`^\s*(楔子|序章|序言|引子|前言|自序|序)\s*$`)

// narrativeUnit is one scene: the text plus where it came from.
//
// ChapterNumber is 0 when no chapter could be identified — 0 means
// UNKNOWN, never "chapter zero". Callers must treat it as "leave the
// chapter column empty", not as a number to compare against (FR-008
// forbids passing a chunk index off as a chapter number).
type narrativeUnit struct {
	Text string
	// Start/End are the BYTE interval this scene occupies in the
	// CRLF-normalized document (010 FR-006). They are exact: a scene is a
	// contiguous slice of the source, because scenesWithin only ever cuts
	// AT divider lines, never inside one.
	Start         int
	End           int
	ChapterNumber int
	ChapterTitle  string
	// SceneIndex is the 1-based position of this scene within its chapter,
	// so a chapter that contains several divider-separated scenes keeps
	// them distinguishable even though they share a chapter number.
	SceneIndex   int
	BoundaryKind string
}

// chapterMark is an identified heading and where it sits.
type chapterMark struct {
	offset int
	number int
	title  string
}

// splitNarrativeScenes turns prose into scenes: chapter boundaries first,
// then explicit dividers inside each chapter.
//
// Text before the first recognised chapter heading becomes its own
// unnumbered scene rather than being dropped — an excerpt may legitimately
// begin mid-book, and silently discarding its opening would lose content
// with no error.
func splitNarrativeScenes(text string) []narrativeUnit {
	return splitNarrativeScenesWithOptions(text, false, true)
}

func splitNarrativeScenesWithOptions(text string, markdown, blankScenes bool) []narrativeUnit {
	marks := findChapterMarksWithOptions(text, markdown)
	if len(marks) == 0 {
		// No chapter structure at all. Every scene is unnumbered; divider
		// splitting still applies (FR-004: degrade, do not fabricate).
		return scenesWithinOptions(text, 0, 0, "", markdown, blankScenes, false)
	}

	var out []narrativeUnit
	if lead := strings.TrimSpace(text[:marks[0].offset]); lead != "" {
		out = append(out, scenesWithinOptions(text[:marks[0].offset], 0, 0, "", markdown, blankScenes, false)...)
	}
	for i, m := range marks {
		end := len(text)
		if i+1 < len(marks) {
			end = marks[i+1].offset
		}
		out = append(out, scenesWithinOptions(text[m.offset:end], m.offset, m.number, m.title, markdown, blankScenes, true)...)
	}
	return out
}

// scenesWithin splits one chapter's body on explicit dividers.
// scenesWithin splits one chapter's body on explicit dividers. base is the
// byte offset of body within the whole document, so the returned units
// carry document-frame intervals.
func scenesWithin(body string, base, chapter int, title string) []narrativeUnit {
	return scenesWithinOptions(body, base, chapter, title, false, true, chapter > 0 || title != "")
}
func scenesWithinOptions(body string, base, chapter int, title string, markdown, blankScenes, hasChapter bool) []narrativeUnit {
	var out []narrativeUnit
	idx := 0
	segStart, offset := 0, 0

	flush := func(segEnd int) {
		text, from, to := trimmedSpan(body, segStart, segEnd)
		if text == "" {
			return
		}
		idx++
		out = append(out, narrativeUnit{
			Text: text, Start: base + from, End: base + to,
			ChapterNumber: chapter, ChapterTitle: title, SceneIndex: idx,
		})
	}
	var fence narrativeFence
	blankCount := 0
	explicit := false
	for _, ln := range strings.SplitAfter(body, "\n") {
		protected := markdown && fence.consume(ln)
		if !protected && sceneDividerPattern.MatchString(strings.TrimSpace(ln)) {
			explicit = true
			// Preserve the marker on the preceding scene; leading markers stay with the next.
			if strings.TrimSpace(body[segStart:offset]) != "" {
				flush(offset + len(ln))
				segStart = offset + len(ln)
			}
			blankCount = 0
		} else if !protected && blankScenes && strings.TrimSpace(ln) == "" {
			blankCount++
			if blankCount == 2 && strings.TrimSpace(body[segStart:offset]) != "" {
				explicit = true
				flush(offset + len(ln))
				segStart = offset + len(ln)
			}
		} else {
			blankCount = 0
		}
		offset += len(ln)
	}
	flush(len(body))
	// ⚠️ kind 是**整段 body 一个值**，不是逐场景一个值，这是有意的，别"修"。
	// 分隔线把 body 切成若干段，所以只要 body 里有一条分隔线，其中每一个
	// 场景都至少有一端**紧贴着**那条真实的分隔线（第一个的尾、最后一个的头、
	// 中间的两头都是）——对它们每一个来说 divider 都是实话。
	// 反过来按"谁起的头"逐场景标才会撒谎：那样一章里第一个场景会被标成
	// chapter_fallback，而 chapter_fallback 的含义是"没做到场景识别，只退到
	// 了章的粒度"——这一章明明识别出了场景。
	kind := boundaryNone
	if hasChapter {
		kind = boundaryChapterFallback
	}
	if explicit {
		kind = boundaryDivider
	}
	for i := range out {
		out[i].BoundaryKind = kind
	}

	return out
}

// headingSubmatch applies the length cap before the pattern, so a long
// paragraph can never be read as a heading no matter how it opens.
func headingSubmatch(line string) []string {
	if len([]rune(strings.TrimSpace(line))) > maxHeadingRunes {
		return nil
	}
	return chapterHeadingPattern.FindStringSubmatch(line)
}

// findChapterMarks locates chapter headings and reports their numbers.
//
// ⭐ It performs a CONTINUITY SELF-CHECK, but a deliberately narrow one:
// it only reports whether the numbers it found run consecutively, and
// never discards headings because of a gap. An uploaded excerpt may
// legitimately start at chapter 11, or skip chapters entirely; throwing
// away every recognised heading because one is missing would turn a
// partially-known structure into a completely unknown one, which is
// strictly worse.
//
// The check exists so a caller can log the anomaly — the failure this
// guards against (a numeral form the pattern does not know, silently
// dropping headings) produces no error on its own.
func findChapterMarks(text string) []chapterMark { return findChapterMarksWithOptions(text, false) }
func findChapterMarksWithOptions(text string, markdown bool) []chapterMark {
	var fence narrativeFence
	var marks []chapterMark
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimRight(line, "\r\n")
		if markdown {
			if fence.consume(line) {
				offset += len(line)
				continue
			}
			trimmed = unwrapNarrativeHeading(trimmed)
		}
		if m := headingSubmatch(trimmed); m != nil {
			if n := chineseNumeral(m[1]); n > 0 {
				marks = append(marks, chapterMark{
					offset: offset, number: n, title: strings.TrimSpace(m[2]),
				})
			}
		} else if prologueHeadingPattern.MatchString(trimmed) {
			// Recognised as a heading, but carries no number — chapter
			// stays unknown rather than invented (FR-004).
			marks = append(marks, chapterMark{offset: offset, number: 0,
				title: strings.TrimSpace(trimmed)})
		}
		offset += len(line)
	}
	return marks
}

// chapterNumbersConsecutive reports whether the identified chapter numbers
// form a gapless ascending run. Callers use it for a WARNING only — see
// findChapterMarks on why a gap must not discard anything.
//
// Unnumbered headings (prologues) are skipped rather than treated as a
// break in the run.
func chapterNumbersConsecutive(marks []chapterMark) bool {
	prev := 0
	for _, m := range marks {
		if m.number == 0 {
			continue
		}
		if prev != 0 && m.number != prev+1 {
			return false
		}
		prev = m.number
	}
	return true
}

var numeralDigits = map[rune]int{
	'〇': 0, '○': 0, '零': 0, '0': 0,
	'一': 1, '1': 1, '两': 2, '二': 2, '2': 2, '三': 3, '3': 3, '四': 4, '4': 4,
	'五': 5, '5': 5, '六': 6, '6': 6, '七': 7, '7': 7, '八': 8, '8': 8,
	'九': 9, '9': 9,
}

// chineseNumeral parses the numeral forms real books actually use, which
// are inconsistent even inside ONE book: 西遊記 writes 第一三回 and
// 第十四回 and 第一五回 and 第十六回, mixing positional digits with the
// 十-based form. Returns 0 when it cannot parse — 0 means unknown.
func chineseNumeral(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if strings.ContainsRune(s, '千') || strings.ContainsRune(s, '百') {
		return numeralWithUnit(s)
	}
	if strings.ContainsRune(s, '十') {
		return numeralWithTen(s)
	}
	// Positional: 一三 = 13, 二○ = 20, 007 = 7.
	n := 0
	for _, r := range s {
		d, ok := numeralDigits[r]
		if !ok {
			return 0
		}
		n = n*10 + d
	}
	return n
}

func numeralWithTen(s string) int {
	head, tail, _ := strings.Cut(s, "十")
	h := 1
	if head != "" {
		if h = chineseNumeral(head); h == 0 {
			return 0
		}
	}
	t := 0
	if tail != "" {
		if t = chineseNumeral(tail); t == 0 && tail != "〇" && tail != "○" && tail != "零" && tail != "0" {
			return 0
		}
	}
	return h*10 + t
}

func numeralWithUnit(s string) int {
	for _, unit := range []struct {
		ch  rune
		mul int
	}{{'千', 1000}, {'百', 100}} {
		if !strings.ContainsRune(s, unit.ch) {
			continue
		}
		head, tail, _ := strings.Cut(s, string(unit.ch))
		h := 1
		if head != "" {
			if h = chineseNumeral(head); h == 0 {
				return 0
			}
		}
		t := 0
		if strings.TrimSpace(tail) != "" {
			if t = chineseNumeral(tail); t == 0 {
				return 0
			}
		}
		return h*unit.mul + t
	}
	return 0
}

// chunkNarrative is the narrative counterpart of chunkPlainText: scenes are
// the structural unit, and each scene is chunked INDEPENDENTLY.
//
// ⭐ It deliberately reuses chunkPlainText rather than reimplementing the
// paragraph → sentence → rune ladder. Two reasons, and the second matters
// more: (1) that ladder is already covered by chunk_test.go, and a parallel
// copy would drift; (2) calling it once PER SCENE is exactly what makes
// overlap scene-local. Overlap is carried inside one chunkPlainText call
// and reset when it returns, so no scene's tail can bleed into the next
// scene's head — which would reintroduce, at the chunk level, the very
// splice this feature exists to remove.
//
// ⚠️ The hard size limit still wins over scene integrity (FR-002/FR-003).
// On both public-domain corpora EVERY scene exceeds the 500-rune default
// (阿Q 9/9, 西遊記 101/101), so in practice the ladder runs on every scene
// — scene boundaries decide WHERE the splits are allowed to fall, they do
// not avoid splitting.
//
// The chapter heading line stays in the first chunk of its chapter (it is
// part of the scene text) and is NOT re-prepended to later chunks the way
// chunkMarkdown does with breadcrumbs. Chapter identity travels as
// metadata (ChapterNumber/SectionTitle/SceneKey) instead, because that is
// what FR-008's downstream consumer reads; duplicating it into every
// chunk's content would spend the rune budget on text the reader of a
// novel does not need repeated.
func chunkNarrative(text string, size, overlap int) []chunkPiece {
	return chunkNarrativeWithOptions(text, size, overlap, false, true)
}
func chunkNarrativeWithOptions(text string, size, overlap int, markdown, blankScenes bool) []chunkPiece {
	size, overlap = normalizeChunkParams(size, overlap)

	// ⭐ Normalize CRLF ONCE, here, and treat the result as the canonical
	// source text. Everything below indexes it. Doing this per-level
	// instead would let paragraph offsets refer to one string and scene
	// offsets to another — the intervals would still look plausible.
	text = strings.ReplaceAll(text, "\r\n", "\n")

	// Built once per document: byte->rune conversion for the persisted
	// offsets, and the hash that identifies this coordinate system.
	ri := newRuneIndex(text)
	docHash := normalizedDocumentHash(text)
	sourceOrder := 0

	var pieces []chunkPiece
	for _, scene := range splitNarrativeScenesWithOptions(text, markdown, blankScenes) {
		// ⚠️ keepClosingQuotes is TRUE for every narrative scene, and must not
		// be keyed off BoundaryKind. Whether a closing 」 belongs to the
		// sentence it closes is a property of the PROSE, not of whether this
		// document happened to have a chapter heading. A version that passed
		// `BoundaryKind != boundaryNone` here split the same text two
		// different ways — an excerpt with no heading (and every PDF whose
		// chapters aren't recognisable) came out with chunks that begin on a
		// dangling ”, which is exactly the defect the flag exists to prevent.
		bodies := shiftSpans(chunkPlainTextSpans(scene.Text, size, overlap, true), scene.Start)
		if len(bodies) == 0 {
			continue
		}
		chapter := scene.ChapterNumber
		key := docHash + ":" + strconv.Itoa(ri.at(scene.Start))
		for _, body := range bodies {
			from, to := body.Start, body.End
			piece := chunkPiece{
				Content:     body.Text,
				SourceStart: &from, SourceEnd: &to,
			}
			if scene.BoundaryKind != boundaryNone {
				piece.SceneKey = &key
			}
			if chapter > 0 {
				// Only a chapter we actually identified is recorded.
				// 0 means unknown and must stay NULL downstream.
				n := chapter
				piece.ChapterNumber = &n
			}
			if scene.ChapterTitle != "" {
				title := scene.ChapterTitle
				piece.SectionTitle = &title
			}
			// source_order is the chunk's linear position in the whole
			// book. ⚠️ It is what everything downstream sorts by — NOT the
			// chapter number, because a flashback chapter's number does not
			// match its position in the text (FR-008).
			meta := buildNarrativeMetadata(ri, docHash, piece, sourceOrder, body, scene.BoundaryKind)
			sourceOrder++
			piece.Narrative = &meta
			pieces = append(pieces, piece)
		}
	}
	return pieces
}
