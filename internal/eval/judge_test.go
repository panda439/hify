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

func TestBuildJudgePrompt_SingleTurnRendersPromptAsBefore(t *testing.T) {
	tc := TestCase{Prompt: "知识库支持哪些文件格式？", Rubric: "标准"}
	prompt := buildJudgePrompt(tc, "回复内容", nil)

	if !strings.Contains(prompt, "用户问题：知识库支持哪些文件格式？") {
		t.Fatalf("single-turn prompt must render 用户问题：<Prompt> unchanged, got:\n%s", prompt)
	}
}

// TestBuildJudgePrompt_MultiTurnIncludesFirstAndLastTurn is the direct
// regression test for the bug this fix addresses: a multi-turn case's
// judge prompt used to be built from tc.Prompt, which is empty when Turns
// is set — the judge never saw the user's actual question(s) at all. It
// must now see every turn, including the first (context the last turn's
// coreference depends on) and the last (the turn actually being scored).
func TestBuildJudgePrompt_MultiTurnIncludesFirstAndLastTurn(t *testing.T) {
	tc := TestCase{
		Turns:  []string{"知识库创建以后还能修改用的向量模型吗？", "那分块大小呢，也不能改吗？"},
		Rubric: "标准",
	}
	prompt := buildJudgePrompt(tc, "分块大小同样不能改", nil)

	if !strings.Contains(prompt, "知识库创建以后还能修改用的向量模型吗？") {
		t.Fatalf("multi-turn prompt must include the first turn (coreference context), got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "那分块大小呢，也不能改吗？") {
		t.Fatalf("multi-turn prompt must include the last turn (the one being scored), got:\n%s", prompt)
	}
	// Must not silently fall back to the empty Prompt field.
	if strings.Contains(prompt, "用户问题：\n") {
		t.Fatalf("multi-turn prompt must not render an empty 用户问题 from the unused Prompt field, got:\n%s", prompt)
	}
}

// TestBuildJudgePrompt_MultiTurnLabelsFinalTurnAsScored guards against
// silently handing the judge an isolated last turn with no indication of
// which turn its "助手最终回复" actually answers — without a label, a
// multi-turn prompt reads ambiguously (which turn does the reply belong
// to?) even though all the turns are present.
func TestBuildJudgePrompt_MultiTurnLabelsFinalTurnAsScored(t *testing.T) {
	tc := TestCase{Turns: []string{"第一轮", "第二轮"}, Rubric: "标准"}
	prompt := buildJudgePrompt(tc, "回复", nil)

	if !strings.Contains(prompt, "最后一轮") {
		t.Fatalf("multi-turn prompt must mark which turn is the one being scored, got:\n%s", prompt)
	}
}
