package conversation

import (
	"fmt"

	"hify/internal/knowledge"
	"hify/internal/provider"
)

const (
	// ragMinSimilarityScore is the hard floor a candidate chunk's cosine
	// similarity score must clear to ever reach the model — see
	// knowledge/pgqueries/chunks.sql's SearchChunks for how the score is
	// computed (1 - cosine distance, so 1.0 = identical, 0.0 = orthogonal,
	// negative = opposite). Below this, a chunk is scored "found something
	// in the vector index" but not "actually relevant to the query" — real
	// embedding models routinely return a same-knowledge-base chunk with a
	// small positive score for a completely unrelated question, and
	// without a floor that chunk would ride retrievalTopK into every
	// answer forever. 0.2 is a deliberately conservative constant (not an
	// admin-configurable setting yet, per CLAUDE.md's Citation V1 scope) —
	// see budget_test.go for the regression pinning irrelevant low-score
	// chunks out of the context.
	ragMinSimilarityScore = 0.2

	// ragBudgetTokens is RAG's own token budget ceiling, independent of
	// the conversation history budget — see ragCapChars. It bounds how
	// much of the fixed per-turn budget selectEvidence is *allowed* to
	// fill, not how much it actually charges: only the evidence that
	// survives selection, rendered exactly as it will be sent (tags,
	// metadata, XML escaping and all — see renderedSourceLen), is ever
	// deducted from history's share. An evidence set that only fills part
	// of this ceiling gives the rest back to history automatically,
	// because history's budget is computed from the real rendered
	// length, not from this constant.
	ragBudgetTokens = 2000

	// toolDefTokenEstimate is a flat per-tool token cost used only to carve
	// out headroom in computeFixedBudget — Hify's char-based budget
	// heuristic (see charsPerToken) has no visibility into the actual
	// serialized size of a provider.ToolDefinition's JSON schema, but
	// ignoring tool definitions entirely would let a model with many
	// attached MCP tools silently blow the context window. This is
	// intentionally rough: a typical tool schema (name + description + a
	// handful of parameters) serializes to well under this many tokens.
	toolDefTokenEstimate = 60
)

// Evidence is one piece of retrieved context that survived similarity
// filtering, dedup, and the RAG budget, and was actually assigned a ref
// (S1, S2, ...) and sent to the model this turn — see selectEvidence. It is
// conversation's own type, not knowledge.RetrievedChunk, because Ref and
// (for a truncated chunk) Content diverge from what knowledge returned:
// ref numbering is a per-turn conversation concern (see model.go's
// "knowledge.Retrieve must not assign refs" rule), and Content here is
// always the raw (unescaped) text actually included — the exact substring
// of the original chunk that ended up inside <source>...</source> before
// XML escaping, which is also exactly what gets persisted as the
// citation's quote. The escaped-for-the-wire version is a rendering
// concern (see formatSource), not something stored anywhere.
type Evidence struct {
	Ref             string
	KnowledgeBaseID string
	DocumentID      string
	// DocumentName is display-ready: knowledge.Chunk.DocumentName falls
	// back to DocumentID here when empty (pre-Citation-V1 chunks whose
	// source document was never reprocessed — see the pgmigrations 000003
	// comment), so nothing downstream needs to repeat that fallback.
	DocumentName string
	ChunkID      string
	ChunkIndex   int
	Content      string
	Score        float64
	PageNumber   *int
	SectionTitle *string
	// Truncated marks a chunk whose Content was rune-safe-cut because its
	// full rendered <source> would not have fit even as the very first
	// piece of evidence — see selectEvidence/truncateEvidenceToFit.
	Truncated bool
}

// totalBudgetChars is the model's whole per-request character allowance
// before anything (system prompt, tools, citation rules, RAG evidence,
// history) is deducted from it — the ceiling estimateRequestChars' output
// must never exceed.
//
// When the model declares a real ContextWindow, that window is an
// absolute hard limit set by the provider — the input budget must never
// exceed max(0, ContextWindow - outputReserveTokens), full stop. An
// earlier version of this function clamped a too-small result up to a
// fixed "reasonable minimum" token count, which was exactly backwards: it
// let Hify silently ask for more input than the model's own window has
// room for once outputReserveTokens is set aside, defeating the point of
// reading ContextWindow at all. defaultContextBudgetTokens is a different
// thing — it's the fallback used only when there's no ContextWindow to be
// bound by in the first place, not a floor applied on top of a real one.
func totalBudgetChars(model provider.Model) int {
	budgetTokens := defaultContextBudgetTokens
	if model.ContextWindow != nil {
		budgetTokens = *model.ContextWindow - outputReserveTokens
		if budgetTokens < 0 {
			budgetTokens = 0
		}
	}
	return budgetTokens * charsPerToken
}

// computeFixedBudget is what's left of totalBudgetChars after the costs
// that exist on every turn regardless of whether this turn has any RAG
// evidence: the Agent's system prompt, the attached tool definitions, and
// the latest user message itself — the last of these matters because the
// latest user message is never optional (see CLAUDE.md's "最新用户消息
// 必须保留" requirement — assembleContext appends it unconditionally,
// never through the same trimmable path older history goes through), so
// its cost is not negotiable the way older history rows are and must be
// reserved up front just like the system prompt.
//
// Unlike an earlier design, it deliberately does NOT reserve anything for
// citation rules or RAG evidence up front — see CLAUDE.md's Citation V1
// review fix: a turn with no knowledge bases, a failed retrieval, or zero
// surviving evidence must get 100% of this budget for history, not have a
// fixed slice carved out that never gets used. assembleContext is
// responsible for deducting the *actual* rendered evidence+rules cost
// (only when evidence is non-empty) to get history's real budget.
//
// required (system prompt + tools + latest message) is the one part of a
// turn's cost that can NEVER be trimmed — RAG evidence gets stripped
// first (down to nothing, via ragCapChars), then older history (down to
// nothing, via truncateByBudget), before this function is ever asked to
// account for anything else. If required alone already exceeds
// totalBudgetChars(model), there is nothing left to cut: silently
// truncating the Agent's system prompt or (worse) the user's actual
// question to force a fit would change what the model is being asked
// without the user's knowledge, and simply proceeding would ask the
// provider for more input than model.ContextWindow allows, which the
// provider would reject anyway (context length exceeded) — so this
// returns ErrContextTooLarge instead of a budget, and the caller
// (assembleContext) must stop immediately: no retrieval, no history
// fetch, no ChatStream call.
func computeFixedBudget(model provider.Model, systemPrompt string, toolCount int, latestUserMessageChars int) (int, error) {
	total := totalBudgetChars(model)
	required := len([]rune(systemPrompt)) + toolCount*toolDefTokenEstimate*charsPerToken + latestUserMessageChars
	if required > total {
		return 0, ErrContextTooLarge
	}
	return total - required, nil
}

// ragCapChars bounds how much of fixedBudget selectEvidence is allowed to
// try to fill — a ceiling for the greedy fill loop, not a charge against
// the budget (see ragBudgetTokens's doc comment for why those are
// different things now).
func ragCapChars(fixedBudget int) int {
	cap := ragBudgetTokens * charsPerToken
	if cap > fixedBudget {
		cap = fixedBudget
	}
	return cap
}

// selectEvidence turns knowledge.Retrieve's candidates into the final,
// numbered evidence set — the only place refs get assigned (see model.go's
// "ref belongs to the conversation turn, not to knowledge" rule). Order is
// preserved from candidates (knowledge.Service.Retrieve already returns
// its cross-KB global topK sorted by score descending), so ref numbering
// naturally follows relevance too.
//
// Pipeline, matching CLAUDE.md's Citation V1 spec (as amended by the
// review fix — fit decisions now use the actual rendered <source> length,
// tags/metadata/XML-escaping included, not raw chunk content length):
//  1. drop anything below ragMinSimilarityScore (filteredByScore)
//  2. dedupe by chunk ID, keeping the first (highest-scored) occurrence
//  3. greedily fill ragCapChars with whole rendered sources in order; a
//     candidate whose rendered size doesn't fit what's left is skipped
//     (filteredByBudget), not partially truncated — except the one case
//     where a candidate's full rendered size already exceeds the *entire*
//     ragCapChars, which would otherwise be impossible to include even as
//     the very first piece of evidence: that one gets a rune-safe
//     truncation (via truncateEvidenceToFit) instead of an outright skip.
//  4. assign S1..Sn in the order candidates were actually included — refs
//     are assigned provisionally as each candidate is considered, since a
//     ref's own length is part of what gets rendered and measured.
//
// A chunk that's filtered at any stage gets no ref and can never
// legitimately appear in the model's answer — normalizeCitations enforces
// that on the output side. ragCapChars is expected to already have the
// <retrieved_sources> wrapper's own overhead subtracted by the caller (see
// context.go's wrapperOverheadChars) — this function only accounts for
// each individual <source> element.
func selectEvidence(candidates []knowledge.RetrievedChunk, ragCapChars int) (evidence []Evidence, filteredByScore, filteredByBudget int) {
	seen := make(map[string]bool, len(candidates))
	remaining := ragCapChars

	for _, c := range candidates {
		if c.Score < ragMinSimilarityScore {
			filteredByScore++
			continue
		}
		if seen[c.ID] {
			continue
		}
		seen[c.ID] = true

		e := Evidence{
			Ref:             fmt.Sprintf("S%d", len(evidence)+1),
			KnowledgeBaseID: c.KnowledgeBaseID,
			DocumentID:      c.DocumentID,
			DocumentName:    evidenceDocumentName(c.Chunk),
			ChunkID:         c.ID,
			ChunkIndex:      c.ChunkIndex,
			Content:         c.Content,
			Score:           c.Score,
			PageNumber:      c.PageNumber,
			SectionTitle:    c.SectionTitle,
		}

		rendered := renderedSourceLen(e)
		switch {
		case rendered <= remaining:
			// fits whole as rendered — tags, metadata, and escaping all
			// accounted for by renderedSourceLen.
		case rendered > ragCapChars:
			// Exceeds the entire cap on its own — the only case that gets
			// a rune-safe partial cut rather than being skipped outright.
			truncatedEvidence, ok := truncateEvidenceToFit(e, remaining)
			if !ok {
				filteredByBudget++
				continue
			}
			e = truncatedEvidence
		default:
			// Fits within the cap in principle, just not what's left
			// after higher-scored evidence already claimed its share —
			// whole-source skip per spec, never a partial cut.
			filteredByBudget++
			continue
		}

		remaining -= renderedSourceLen(e)
		evidence = append(evidence, e)
	}

	return evidence, filteredByScore, filteredByBudget
}

// renderedSourceLen is the exact rune length of formatSource(e) — the one
// function both selectEvidence's fit decisions and truncateEvidenceToFit's
// binary search use, so "will this fit" and "what actually gets sent" can
// never drift apart (the review fix this closes: the old budget only
// counted raw chunk content, not the <source> tag, ref/document/section
// attributes, or XML-escaping expansion around it).
func renderedSourceLen(e Evidence) int {
	return len([]rune(formatSource(e)))
}

// truncateEvidenceToFit binary-searches the largest rune-safe prefix of
// e.Content whose *rendered* <source> element (tag + escaped body) still
// fits budgetChars — not the largest raw-content prefix, since XML
// escaping can expand a handful of characters (&, <, >) into several each,
// and measuring the wrong thing is exactly the bug this function exists to
// avoid. Every candidate measured during the search already has
// Truncated=true set — the final result always will too, and the
// `truncated="true"` attribute formatSource adds for that flag is itself
// a few more characters that must be counted during the search, not
// tacked on afterward (an earlier version of this function measured with
// Truncated=false during the search and only set it true on the result,
// which could push the "fitting" answer's real rendered length past
// budgetChars by exactly that attribute's width). Returns ok=false when
// even an empty-content, already-marked-truncated <source> wouldn't fit,
// meaning this piece of evidence can't be included at all under the
// remaining budget.
func truncateEvidenceToFit(e Evidence, budgetChars int) (Evidence, bool) {
	empty := e
	empty.Content = ""
	empty.Truncated = true
	if renderedSourceLen(empty) > budgetChars {
		return Evidence{}, false
	}

	runes := []rune(e.Content)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		candidate := e
		candidate.Content = string(runes[:mid])
		candidate.Truncated = true
		if renderedSourceLen(candidate) <= budgetChars {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	e.Content = string(runes[:lo])
	e.Truncated = true
	return e, true
}

// evidenceDocumentName is the one place the "empty DocumentName -> fall
// back to DocumentID" rule from knowledge.Chunk's doc comment gets applied
// — see CLAUDE.md's Citation V1 spec section 3.2: a pre-migration chunk
// with no document_name snapshot must still produce a citation with *some*
// identifiable (never fabricated) source label.
func evidenceDocumentName(c knowledge.Chunk) string {
	if c.DocumentName != "" {
		return c.DocumentName
	}
	return c.DocumentID
}

// estimateRequestChars is the same char-based heuristic (see
// charsPerToken) applied to an actual, fully-assembled request — a
// testable way to assert assembleContext's real output never exceeds the
// budget it was built from, rather than trusting each intermediate piece
// individually added up correctly.
func estimateRequestChars(messages []provider.Message, tools []provider.ToolDefinition) int {
	total := 0
	for _, m := range messages {
		total += len([]rune(m.Content))
	}
	total += len(tools) * toolDefTokenEstimate * charsPerToken
	return total
}
