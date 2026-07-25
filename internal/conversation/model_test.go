package conversation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// --- 第一轮代码审查修复：问题五（final 事件的 citations 契约稳定性） ---

func TestStreamEventJSONFinalAlwaysHasCitationsArray(t *testing.T) {
	cases := []struct {
		name string
		evt  StreamEvent
		want string // substring that must be present
	}{
		{
			name: "final with nil citations serializes to empty array, not null or omitted",
			evt:  StreamEvent{Type: EventFinal, Content: "答案"},
			want: `"citations":[]`,
		},
		{
			name: "final with citations serializes as a structured array",
			evt: StreamEvent{Type: EventFinal, Content: "答案[S1]", Citations: []CitationResponse{
				{Ref: "S1", DocumentName: "a.md"},
			}},
			want: `"citations":[{"ref":"S1"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.evt)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !strings.Contains(string(b), tc.want) {
				t.Fatalf("json = %s, want to contain %q", b, tc.want)
			}
			// content must never disappear for a final event either.
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(b, &decoded); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if _, ok := decoded["citations"]; !ok {
				t.Fatalf("json = %s, missing citations key entirely", b)
			}
		})
	}
}

func TestStreamEventJSONNonFinalEventsNeverCarryCitationsField(t *testing.T) {
	cases := []StreamEvent{
		{Type: EventRetrieval, Retrieved: []RetrievedChunkInfo{{Ref: "S1"}}},
		{Type: EventDelta, Content: "片段"},
		{Type: EventToolCall, ToolCall: &ToolCallInfo{Name: "x", Status: "running"}},
		{Type: EventDone},
		{Type: EventError, Error: "出错了"},
	}
	for _, evt := range cases {
		t.Run(evt.Type, func(t *testing.T) {
			b, err := json.Marshal(evt)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(b, &decoded); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if _, ok := decoded["citations"]; ok {
				t.Fatalf("event type %q must not carry a citations field at all, got json = %s", evt.Type, b)
			}
		})
	}
}

// formatSSEFrame mirrors handler.go's SendMessage exactly (event: %s\ndata:
// %s\n\n) — kept in the test rather than exported from handler.go, since
// handler.go has no reason to expose it outside its one Stream callback.
// This is what lets this test assert on "the real SSE bytes a client would
// receive", not just the JSON payload in isolation.
func formatSSEFrame(evt StreamEvent) (string, error) {
	payload, err := json.Marshal(evt)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("event: %s\ndata: %s\n\n", evt.Type, payload), nil
}

func TestSSEFrameFinalContractMatchesSpec(t *testing.T) {
	frame, err := formatSSEFrame(StreamEvent{Type: EventFinal, Content: "答案是这样的"})
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "event: final\ndata: "
	if !strings.HasPrefix(frame, wantPrefix) {
		t.Fatalf("frame = %q, want prefix %q", frame, wantPrefix)
	}
	if !strings.Contains(frame, `"citations":[]`) {
		t.Fatalf("frame = %q, want citations:[] present per the stable SSE contract", frame)
	}
	if !strings.HasSuffix(frame, "\n\n") {
		t.Fatalf("frame = %q, want trailing blank line (SSE frame terminator)", frame)
	}
}
