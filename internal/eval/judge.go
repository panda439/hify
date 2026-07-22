package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"hify/internal/platform/trace"
	"hify/internal/provider"
)

// judgeVerdict is the JSON shape the judge model is instructed to reply
// with — parsed strictly: a judge reply that doesn't parse is a failed
// case (see runCase), never silently scored as a default value.
type judgeVerdict struct {
	Score     int    `json:"score"`
	Reasoning string `json:"reasoning"`
}

// Judge asks judgeClient to score one case's reply against its rubric,
// given the trace of what the agent actually did (which tool calls fired,
// what retrieval returned) — the rubric can require "must have called the
// weather tool", and the judge can only verify that from the trace, not
// from the reply text alone.
func Judge(ctx context.Context, judgeClient provider.Client, judgeModel string, tc TestCase, reply string, spans []trace.Span) (score int, reasoning string, err error) {
	prompt := buildJudgePrompt(tc, reply, spans)
	msg, err := judgeClient.Chat(ctx, provider.ChatRequest{
		Model:    judgeModel,
		Messages: []provider.Message{{Role: provider.RoleUser, Content: prompt}},
	})
	if err != nil {
		return 0, "", fmt.Errorf("eval: judge chat call: %w", err)
	}

	var verdict judgeVerdict
	if err := json.Unmarshal([]byte(extractJSON(msg.Content)), &verdict); err != nil {
		return 0, "", fmt.Errorf("eval: parse judge verdict %q: %w", msg.Content, err)
	}
	if verdict.Score < 1 || verdict.Score > 5 {
		return 0, "", fmt.Errorf("eval: judge score %d out of range 1-5", verdict.Score)
	}
	return verdict.Score, verdict.Reasoning, nil
}

func buildJudgePrompt(tc TestCase, reply string, spans []trace.Span) string {
	var sb strings.Builder
	sb.WriteString("你是一个严格的 AI 助手回复质量评审员。请根据评分标准给这次对话打 1-5 分（5 分最好），只输出 JSON，不要输出其它任何文字：{\"score\": <1-5的整数>, \"reasoning\": \"<简短中文理由>\"}\n\n")
	fmt.Fprintf(&sb, "用户问题：%s\n\n", tc.Prompt)
	fmt.Fprintf(&sb, "评分标准：%s\n\n", tc.Rubric)
	fmt.Fprintf(&sb, "助手最终回复：%s\n\n", reply)
	sb.WriteString("执行过程（供核实评分标准里提到的工具调用/检索是否真的发生）：\n")
	for _, sp := range spans {
		fmt.Fprintf(&sb, "- [%s] %s status=%s\n", sp.Kind, sp.Name, sp.Status)
	}
	return sb.String()
}

// extractJSON strips a ```json fenced block if the judge wrapped its
// answer in one despite being asked not to — some models do this
// regardless of instruction, and failing the case over pure formatting
// noise would make the harness flaky rather than informative.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}
