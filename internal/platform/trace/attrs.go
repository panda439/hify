package trace

import "encoding/json"

// Attribute keys follow the OpenTelemetry GenAI semantic conventions
// (gen_ai.*) so a span's Attrs blob stays compatible with off-the-shelf
// tracing backends (Jaeger/Langfuse/etc.) if one is ever wired up later —
// this package itself does no exporting, it only picks field names that
// wouldn't need remapping if that happens.
const (
	AttrRequestModel      = "gen_ai.request.model"
	AttrUsageInputTokens  = "gen_ai.usage.input_tokens"
	AttrUsageOutputTokens = "gen_ai.usage.output_tokens"
	AttrFinishReasons     = "gen_ai.response.finish_reasons"
	AttrToolName          = "gen_ai.tool.name"

	// Citation V1 counts — deliberately metadata only. Span.Input/Output
	// for retrieval/llm_call/tool_call spans deliberately do NOT carry the
	// full retrieved text, user question, system prompt, model answer, or
	// tool result (see CLAUDE.md's "trace 默认不要新增完整原文副本" rule
	// and the code-review fix that removed the earlier full-text copies) —
	// these Attrs are the only place this information survives in a span,
	// and only as counts/ids/lengths, never content.
	AttrRetrievedCount        = "hify.rag.retrieved_count"
	AttrFilteredByScoreCount  = "hify.rag.filtered_by_score_count"
	AttrFilteredByBudgetCount = "hify.rag.filtered_by_budget_count"
	AttrValidCitationCount    = "hify.rag.valid_citation_count"
	AttrInvalidCitationCount  = "hify.rag.invalid_citation_count"
	// AttrEvidence holds a JSON array of per-evidence metadata (ref,
	// knowledge_base_id, document_id, chunk_id, score, content_length) —
	// enough to trace which sources were used and how big they were,
	// without ever storing the source text itself.
	AttrEvidence = "hify.rag.evidence"
	// AttrQueryLength is len(latestUserMessage) in runes — a size signal
	// for debugging retrieval behavior without persisting the question.
	AttrQueryLength = "hify.rag.query_length"

	// AttrMessageCount/AttrInputLength/AttrOutputLength back the llm_call
	// span now that Input/Output no longer carry the full request/answer
	// text — message count and total content length are enough to spot a
	// request that's abnormally large without ever storing what's in it.
	AttrMessageCount = "hify.request.message_count"
	AttrInputLength  = "hify.request.input_length"
	AttrOutputLength = "hify.response.output_length"
)

// Attrs builds a Span.Attrs value from a set of key/value pairs, omitting
// any key whose value is the zero value — callers pass usage/finish-reason
// fields unconditionally and let this function drop what wasn't available
// (e.g. a provider that didn't return token usage), rather than every call
// site having to build the map conditionally itself.
func Attrs(kv map[string]any) json.RawMessage {
	clean := make(map[string]any, len(kv))
	for k, v := range kv {
		switch val := v.(type) {
		case string:
			if val == "" {
				continue
			}
		case int:
			if val == 0 {
				continue
			}
		}
		clean[k] = v
	}
	if len(clean) == 0 {
		return json.RawMessage("{}")
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}
