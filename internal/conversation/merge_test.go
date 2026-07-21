package conversation

import (
	"encoding/json"
	"testing"

	"hify/internal/provider"
)

// Characterization tests for mergeToolCallDeltas — 链路 1 的关键纯逻辑：
// OpenAI 流式协议把一个 tool call 的 arguments 拆到多个 chunk（且多个
// call 可交错），只有首个分片带 ID/Name，合并键是 Index。改坏的表现是
// tool call 参数 JSON 残缺或串号，落库和工具执行才暴露。

func ip(i int) *int { return &i }

func tc(idx *int, id, name, args string) provider.ToolCall {
	return provider.ToolCall{Index: idx, ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func TestMergeSingleCallAcrossChunks(t *testing.T) {
	var acc []provider.ToolCall
	acc = mergeToolCallDeltas(acc, []provider.ToolCall{tc(ip(0), "call_1", "get_weather", `{"city":`)})
	acc = mergeToolCallDeltas(acc, []provider.ToolCall{tc(ip(0), "", "", `"北京"}`)})

	if len(acc) != 1 {
		t.Fatalf("len = %d, want 1", len(acc))
	}
	got := acc[0]
	if got.ID != "call_1" || got.Name != "get_weather" {
		t.Fatalf("ID/Name lost after merge: %+v", got)
	}
	if string(got.Arguments) != `{"city":"北京"}` {
		t.Fatalf("Arguments = %s, want spliced JSON", got.Arguments)
	}
}

func TestMergeInterleavedCalls(t *testing.T) {
	var acc []provider.ToolCall
	acc = mergeToolCallDeltas(acc, []provider.ToolCall{
		tc(ip(0), "call_a", "tool_a", `{"a"`),
		tc(ip(1), "call_b", "tool_b", `{"b"`),
	})
	acc = mergeToolCallDeltas(acc, []provider.ToolCall{
		tc(ip(1), "", "", `:2}`),
		tc(ip(0), "", "", `:1}`),
	})

	if len(acc) != 2 {
		t.Fatalf("len = %d, want 2", len(acc))
	}
	if string(acc[0].Arguments) != `{"a":1}` || acc[0].ID != "call_a" {
		t.Fatalf("call 0 corrupted: %+v", acc[0])
	}
	if string(acc[1].Arguments) != `{"b":2}` || acc[1].ID != "call_b" {
		t.Fatalf("call 1 corrupted: %+v", acc[1])
	}
}

func TestMergeNilIndexDefaultsToZero(t *testing.T) {
	var acc []provider.ToolCall
	acc = mergeToolCallDeltas(acc, []provider.ToolCall{tc(nil, "call_1", "f", `{`)})
	acc = mergeToolCallDeltas(acc, []provider.ToolCall{tc(nil, "", "", `}`)})

	if len(acc) != 1 || string(acc[0].Arguments) != `{}` {
		t.Fatalf("nil Index should merge into slot 0: %+v", acc)
	}
}

func TestMergeIndexGapCreatesEmptySlots(t *testing.T) {
	// 现状行为：首个分片 Index=2 时，槽 0/1 以零值占位。锁住它以防
	// 改造时无意变成 panic 或吞掉分片。
	acc := mergeToolCallDeltas(nil, []provider.ToolCall{tc(ip(2), "call_c", "f", `{}`)})
	if len(acc) != 3 {
		t.Fatalf("len = %d, want 3 (two placeholder slots)", len(acc))
	}
	if acc[0].ID != "" || acc[1].ID != "" || acc[2].ID != "call_c" {
		t.Fatalf("unexpected slot layout: %+v", acc)
	}
}

func TestMergeLaterIDNameDoNotOverwriteWithEmpty(t *testing.T) {
	acc := mergeToolCallDeltas(nil, []provider.ToolCall{tc(ip(0), "call_1", "f", ``)})
	acc = mergeToolCallDeltas(acc, []provider.ToolCall{tc(ip(0), "", "", `{}`)})
	if acc[0].ID != "call_1" || acc[0].Name != "f" {
		t.Fatalf("empty ID/Name on later chunk must not clear earlier values: %+v", acc[0])
	}
}
