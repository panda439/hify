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
	boundaryChapter         = "chapter" // legacy metadata
	boundaryChapterFallback = "chapter_fallback"
	boundaryDivider         = "divider"
	boundaryNone            = "none"
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

	// IsOverlapCopy marks copied context; its original interval is retained for citation deduplication.
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

// buildNarrativeMetadata persists the source map carried through every split/join.
func buildNarrativeMetadata(ri *runeIndex, docHash string, piece chunkPiece, sourceOrder int, body textSpan, kind string) narrativeMetadata {
	return narrativeMetadata{SchemaVersion: narrativeMetadataSchemaVersion,
		NormalizedDocumentHash: docHash, BoundaryKind: kind, SceneKey: piece.SceneKey,
		ChapterNumber: piece.ChapterNumber, ChapterTitle: piece.SectionTitle, SourceOrder: sourceOrder,
		Segments: sourceSegments(ri, body)}
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
	case boundaryChapter, boundaryChapterFallback, boundaryDivider, boundaryNone:
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

		locatable := !seg.IsGeneratedSeparator
		if (seg.DocumentStart == nil) != (seg.DocumentEnd == nil) {
			return fmt.Errorf("%w: partial source interval", errMetadataRange)
		}
		hasInterval := seg.DocumentStart != nil && seg.DocumentEnd != nil
		if locatable && !hasInterval {
			return fmt.Errorf("%w: segment %d", errMetadataUnlocated, i)
		}
		if hasInterval {
			if *seg.DocumentStart < 0 || *seg.DocumentStart >= *seg.DocumentEnd || *seg.DocumentEnd-*seg.DocumentStart != seg.ChunkEnd-seg.ChunkStart {
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
