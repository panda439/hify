package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/sqlc-dev/pqtype"
)

// narrative_metadata.go turns a narrative chunk's byte spans into the
// persisted source map (010 FR-006, data-model §4), and validates it.
//
// ⭐ Why the two coordinate systems differ, deliberately:
// the chunker works in BYTE offsets (Go slices by bytes; rune offsets would
// cost an O(n) scan per unit), while what gets STORED is RUNE offsets —
// data-model §4 fixes them as 0-based half-open rune intervals. A rune
// offset is a character position a human or a model can reason about;
// a byte offset in Chinese text is three times the number it looks like.
// The conversion is one linear pass per document, done here at the
// persistence boundary and nowhere else.
//
// ⚠️ Everything here is a pure function. The metadata is written in the
// same transaction as the chunk (repository.createChunks), but nothing in
// this file touches a database.

const narrativeMetadataSchemaVersion = 1

// Boundary kinds — what decided this chunk's scene.
const (
	boundaryChapter = "chapter"
	boundaryDivider = "divider"
	boundaryNone    = "none"
)

// narrativeSegment maps one stretch of a chunk's CONTENT back to the
// normalized source document. All offsets are 0-based half-open RUNE
// offsets; Page is the only 1-based number in the structure.
type narrativeSegment struct {
	ChunkStart int `json:"chunk_start"`
	ChunkEnd   int `json:"chunk_end"`

	// DocumentStart/End are nil exactly when this stretch has no single
	// place in the document it can honestly be attributed to — see
	// IsOverlapCopy and IsGeneratedSeparator below.
	//
	// ⚠️ nil means "not locatable", never 0. Writing 0 would make an
	// un-citable stretch look like it came from the top of the book.
	DocumentStart *int `json:"document_start"`
	DocumentEnd   *int `json:"document_end"`

	Page *int `json:"page"`

	// IsGeneratedSeparator marks characters the system spliced in (join
	// separators between merged units). They are not in the source, so
	// they can never be quoted as evidence.
	IsGeneratedSeparator bool `json:"is_generated_separator"`

	// IsOverlapCopy marks the seed carried over from the previous chunk.
	// The text is real, but its home is the PREVIOUS chunk's range —
	// citing it from here would point a reader at the wrong place, so it
	// carries no document interval at all.
	IsOverlapCopy bool `json:"is_overlap_copy"`
}

// narrativeMetadata is the JSONB payload on chunks.narrative_metadata.
type narrativeMetadata struct {
	SchemaVersion int    `json:"schema_version"`
	BoundaryKind  string `json:"boundary_kind"`
	// NormalizedDocumentHash identifies the coordinate system the offsets
	// belong to. ⚠️ Without it, offsets from a re-processed document look
	// exactly like offsets from the current one — a citation that resolves
	// cleanly to the wrong text.
	NormalizedDocumentHash string             `json:"normalized_document_hash"`
	SceneKey               *string            `json:"scene_key"`
	ChapterNumber          *int               `json:"chapter_number"`
	ChapterTitle           *string            `json:"chapter_title"`
	SourceOrder            int                `json:"source_order"`
	Segments               []narrativeSegment `json:"segments"`
}

// runeIndex converts byte offsets into rune offsets for one document.
// Built in a single pass; every lookup is O(1).
type runeIndex struct {
	byteToRune []int // len(text)+1 entries
}

func newRuneIndex(text string) *runeIndex {
	idx := &runeIndex{byteToRune: make([]int, len(text)+1)}
	r := 0
	for b := 0; b < len(text); {
		_, size := utf8.DecodeRuneInString(text[b:])
		for i := 0; i < size; i++ {
			idx.byteToRune[b+i] = r
		}
		b += size
		r++
	}
	idx.byteToRune[len(text)] = r
	return idx
}

func (ri *runeIndex) at(byteOffset int) int {
	if byteOffset < 0 {
		return 0
	}
	if byteOffset >= len(ri.byteToRune) {
		return ri.byteToRune[len(ri.byteToRune)-1]
	}
	return ri.byteToRune[byteOffset]
}

func normalizedDocumentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// buildNarrativeMetadata describes one chunk's provenance.
//
// contentRunes is the chunk's own rune length; prefixRunes is how many of
// its leading runes are an overlap copy (see textSpan.PrefixRunes).
//
// ⭐ 每个 segment 是原文的**精确切片**，逐字节相等，不只是边界对齐。
//
// ⚠️ 这条更正了本文件早先的一个错误说法。原先整块一段，而块的区间覆盖了
// 单元**之间的原文空白**，块文本却是用固定分隔符重新拼接的——两者长度
// 必然不等（实测 149 rune 的块对应 126 rune 的文档区间），当时只能说
// "边界精确、内部可能差空白"，并要求下游忽略空白比对。
//
// 那个说法掩盖了真正的问题：内部长度不等意味着**区间内的位置换算全部无效**，
// 而引用定位正是靠位置换算。改成一个源单元一段之后，每段长度精确相等，
// 换算成立，下游可以逐字节比对。
// （实测：阿Q正传 319 段、西遊記全 100 回 29511 段，逐字节 0 处不符。）
func buildNarrativeMetadata(
	ri *runeIndex, docHash string, piece chunkPiece, sourceOrder, contentRunes, prefixRunes int,
	units []textSpan, sepRunes int,
) narrativeMetadata {
	_ = contentRunes // 覆盖完整性由 validateNarrativeMetadata 核对
	meta := narrativeMetadata{
		SchemaVersion:          narrativeMetadataSchemaVersion,
		NormalizedDocumentHash: docHash,
		BoundaryKind:           boundaryNone,
		SceneKey:               piece.SceneKey,
		ChapterNumber:          piece.ChapterNumber,
		ChapterTitle:           piece.SectionTitle,
		SourceOrder:            sourceOrder,
	}
	if piece.ChapterNumber != nil {
		meta.BoundaryKind = boundaryChapter
	} else if piece.SceneKey != nil {
		meta.BoundaryKind = boundaryDivider
	}

	if prefixRunes > 0 {
		meta.Segments = append(meta.Segments, narrativeSegment{
			ChunkStart: 0, ChunkEnd: prefixRunes, IsOverlapCopy: true,
		})
	}

	// ⭐ 一个源单元一段，而不是整块一段。
	//
	// ⚠️ 整块一段是错的，而且错得很隐蔽：块的 [Start,End) 覆盖了单元**之间
	// 的原文空白**，而块文本是用固定分隔符重新拼接的，两者长度必然不等
	// （实测 149 rune 的块对应 126 rune 的文档区间）。据此做位置换算，
	// 每一条引用的位置都会偏，偏移量取决于原文里那些空白有多长——
	// 没有任何规律可循。按单元切段之后每一段都是原文的精确切片。
	//
	// 单元之间的拼接符标成 is_generated_separator：它们不在原文里，
	// 不可引用，而 segments 又必须完整覆盖块内容。
	cursor := prefixRunes
	for i, unit := range units {
		if i > 0 && sepRunes > 0 {
			meta.Segments = append(meta.Segments, narrativeSegment{
				ChunkStart: cursor, ChunkEnd: cursor + sepRunes,
				IsGeneratedSeparator: true,
			})
			cursor += sepRunes
		}
		n := len([]rune(unit.Text))
		from, to := ri.at(unit.Start), ri.at(unit.End)
		meta.Segments = append(meta.Segments, narrativeSegment{
			ChunkStart: cursor, ChunkEnd: cursor + n,
			DocumentStart: &from, DocumentEnd: &to,
		})
		cursor += n
	}
	return meta
}

var (
	errMetadataSchema    = errors.New("narrative metadata: unsupported schema version")
	errMetadataBoundary  = errors.New("narrative metadata: unknown boundary kind")
	errMetadataCoverage  = errors.New("narrative metadata: segments do not cover the content")
	errMetadataRange     = errors.New("narrative metadata: invalid interval")
	errMetadataUnlocated = errors.New("narrative metadata: locatable segment without a document interval")
)

// validateNarrativeMetadata enforces data-model §4's invariants.
//
// ⭐ The coverage check is the one that matters. Every other field being
// well-formed still permits a chunk whose content is only PARTLY mapped —
// and a partly-mapped chunk produces citations that are correct for the
// part that happens to be covered and silently wrong for the rest. There is
// no runtime symptom; the numbers just come out slightly off.
func validateNarrativeMetadata(meta narrativeMetadata, contentRunes int) error {
	if meta.SchemaVersion != narrativeMetadataSchemaVersion {
		return fmt.Errorf("%w: %d", errMetadataSchema, meta.SchemaVersion)
	}
	switch meta.BoundaryKind {
	case boundaryChapter, boundaryDivider, boundaryNone:
	default:
		return fmt.Errorf("%w: %q", errMetadataBoundary, meta.BoundaryKind)
	}
	if meta.SourceOrder < 0 {
		return fmt.Errorf("%w: source_order %d", errMetadataRange, meta.SourceOrder)
	}
	if meta.ChapterNumber != nil && *meta.ChapterNumber < 1 {
		// 0 is UNKNOWN and must be expressed as nil, never as chapter 0.
		return fmt.Errorf("%w: chapter_number %d", errMetadataRange, *meta.ChapterNumber)
	}

	cursor := 0
	for i, seg := range meta.Segments {
		if seg.ChunkStart < 0 || seg.ChunkStart >= seg.ChunkEnd {
			return fmt.Errorf("%w: segment %d chunk [%d,%d)", errMetadataRange, i, seg.ChunkStart, seg.ChunkEnd)
		}
		// Ordered and gapless: segments must tile the content, in order.
		if seg.ChunkStart != cursor {
			return fmt.Errorf("%w: segment %d starts at %d, expected %d",
				errMetadataCoverage, i, seg.ChunkStart, cursor)
		}
		cursor = seg.ChunkEnd

		locatable := !seg.IsOverlapCopy && !seg.IsGeneratedSeparator
		hasInterval := seg.DocumentStart != nil && seg.DocumentEnd != nil
		if locatable && !hasInterval {
			return fmt.Errorf("%w: segment %d", errMetadataUnlocated, i)
		}
		if hasInterval {
			if *seg.DocumentStart < 0 || *seg.DocumentStart >= *seg.DocumentEnd {
				return fmt.Errorf("%w: segment %d document [%d,%d)",
					errMetadataRange, i, *seg.DocumentStart, *seg.DocumentEnd)
			}
			if !locatable {
				// ⚠️ An un-citable stretch carrying a document interval is
				// worse than one carrying none: it invites a citation that
				// points somewhere plausible and wrong.
				return fmt.Errorf("%w: segment %d is not citable but carries an interval",
					errMetadataRange, i)
			}
		}
		if seg.Page != nil && *seg.Page < 1 {
			return fmt.Errorf("%w: segment %d page %d", errMetadataRange, i, *seg.Page)
		}
	}
	if cursor != contentRunes {
		return fmt.Errorf("%w: covered %d of %d runes", errMetadataCoverage, cursor, contentRunes)
	}
	return nil
}

// encodeNarrativeMetadata marshals metadata for the JSONB column.
// nil in, SQL NULL out — that is what keeps every non-narrative chunk's
// stored value exactly as it was before this column existed.
func encodeNarrativeMetadata(meta *narrativeMetadata) (pqtype.NullRawMessage, error) {
	if meta == nil {
		return pqtype.NullRawMessage{}, nil
	}
	blob, err := json.Marshal(meta)
	if err != nil {
		return pqtype.NullRawMessage{}, fmt.Errorf("marshal narrative metadata: %w", err)
	}
	return pqtype.NullRawMessage{RawMessage: blob, Valid: true}, nil
}

// decodeNarrativeMetadata reads the column back.
//
// ⚠️ A corrupt or unknown-schema value is an ERROR here, not a silent nil.
// This is deliberately the opposite of notice.go's page lists, which
// degrade to "no notice" on corruption: a missing notice costs the user one
// piece of advice, whereas a chunk that silently loses its source map still
// gets retrieved and still gets cited — pointing nowhere verifiable.
func decodeNarrativeMetadata(raw pqtype.NullRawMessage) (*narrativeMetadata, error) {
	if !raw.Valid || len(raw.RawMessage) == 0 {
		return nil, nil
	}
	var meta narrativeMetadata
	if err := json.Unmarshal(raw.RawMessage, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal narrative metadata: %w", err)
	}
	if meta.SchemaVersion != narrativeMetadataSchemaVersion {
		return nil, fmt.Errorf("%w: %d", errMetadataSchema, meta.SchemaVersion)
	}
	return &meta, nil
}
