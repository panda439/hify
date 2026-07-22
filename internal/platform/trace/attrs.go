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
