package eval

import (
	"strings"
	"testing"
)

func TestBuildJudgePrompt_DoesNotClaimAccessToRedactedTraceContent(t *testing.T) {
	tc := TestCase{
		Prompt:         "问题",
		Rubric:         "标准",
		ExpectedFacts:  []string{"必须提到 pgvector"},
		ForbiddenFacts: []string{"不能提到 Milvus"},
	}
	prompt := buildJudgePrompt(tc, "回复内容", nil)

	// The prompt must state the trace no longer carries retrieved text,
	// the raw user question, or tool call args/results — never claim the
	// judge can inspect them.
	if !strings.Contains(prompt, "不包含检索到的文档原文") {
		t.Fatalf("prompt must disclose that spans carry no retrieved text, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "用户问题原文") {
		t.Fatalf("prompt must disclose that spans carry no raw user question, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "工具调用参数或结果") {
		t.Fatalf("prompt must disclose that spans carry no tool call args/results, got:\n%s", prompt)
	}
	// Must not resurrect the old, now-false claim that the judge can
	// verify retrieval *content* correctness from the trace.
	if strings.Contains(prompt, "检索内容是否正确") {
		t.Fatalf("prompt must not claim retrieval content can be checked from the trace, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "必须提到 pgvector") {
		t.Fatalf("prompt must include ExpectedFacts, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "不能提到 Milvus") {
		t.Fatalf("prompt must include ForbiddenFacts, got:\n%s", prompt)
	}
}
