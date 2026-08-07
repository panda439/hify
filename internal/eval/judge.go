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
// expected/forbidden facts, and the trace of which tool calls or retrieval
// fired (kind/status/timing only — see buildJudgePrompt's prompt text: the
// trace no longer carries retrieved text, the user's question, or tool
// call arguments/results after the privacy rewrite, and this prompt does
// not claim otherwise). Judge is responsible for the semantic calls that
// can't be done with plain string/id comparison — fact coverage, forbidden
// content, answer relevance/quality — deterministic retrieval/citation
// correctness is computeRAGMetrics' job, not this prompt's (see runner.go).
// V1 does not attempt a general faithfulness score (checking the reply
// against the full retrieved text) — that would require re-exposing
// retrieved content to the judge, which the privacy design in eval's
// package doc rules out.
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
	sb.WriteString(formatJudgeTurns(tc))
	fmt.Fprintf(&sb, "评分标准：%s\n\n", tc.Rubric)
	if len(tc.ExpectedFacts) > 0 {
		sb.WriteString("回复中应当正确表达以下事实点（缺失或表达错误要在打分中体现）：\n")
		for _, f := range tc.ExpectedFacts {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
		sb.WriteString("\n")
	}
	if len(tc.ForbiddenFacts) > 0 {
		sb.WriteString("回复中不应当出现以下内容（出现要在打分中体现）：\n")
		for _, f := range tc.ForbiddenFacts {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "助手最终回复：%s\n\n", reply)
	sb.WriteString("执行过程（只能用于核实评分标准/事实点里提到的工具调用或检索这类动作是否真的发生——每个 span 只有状态、耗时等元数据，不包含检索到的文档原文、用户问题原文、工具调用参数或结果，这些已经在 Trace 隐私改造中移除；不要假设能看到或还原这些被删除的原文，也不要仅仅因为看不到原文就扣分，检索/引用是否命中期望文档已由确定性指标单独判断，不需要你在这里核实）：\n")
	for _, sp := range spans {
		fmt.Fprintf(&sb, "- [%s] %s status=%s\n", sp.Kind, sp.Name, sp.Status)
		if in := truncateForJudge(sp.Input); in != "" {
			fmt.Fprintf(&sb, "  input: %s\n", in)
		}
		if out := truncateForJudge(sp.Output); out != "" {
			fmt.Fprintf(&sb, "  output: %s\n", out)
		}
	}
	return sb.String()
}

// formatJudgeTurns renders the conversation's user-side prompt(s) for the
// judge. Single-turn cases (TestCase.Turns unset) render exactly as
// before — just "用户问题：<prompt>". Multi-turn cases render *every* turn
// in order, not just the last one: handing the judge an isolated final
// turn like "那分块大小呢" without the turn it refers back to would strand
// it without the coreference context that question depends on, defeating
// the point of testing coreference at all. The final turn is labeled
// explicitly as the one being scored, since only its
// reply/retrievals/citations feed into Score/Metrics (see runCase and
// TestCase.Turns' doc comment) — earlier turns are conversational context
// for the judge to read, not something it should be grading on their own
// merits.
func formatJudgeTurns(tc TestCase) string {
	turns := caseTurns(tc)
	if len(turns) == 1 {
		return fmt.Sprintf("用户问题：%s\n\n", turns[0])
	}

	var sb strings.Builder
	sb.WriteString("多轮对话（同一个会话里按顺序发生，后面的问题可能用代词或省略指代前面的内容，请结合上下文理解）：\n")
	for i, turn := range turns {
		if i == len(turns)-1 {
			fmt.Fprintf(&sb, "第 %d 轮（最后一轮，下面的“助手最终回复”是对这一轮的回复，只评这一轮）：%s\n", i+1, turn)
		} else {
			fmt.Fprintf(&sb, "第 %d 轮：%s\n", i+1, turn)
		}
	}
	sb.WriteString("\n")
	return sb.String()
}

// judgeSpanFieldLimit caps how much of a span's Input/Output goes into the
// judge prompt — enough to verify tool-call arguments and retrieval
// relevance, without letting one large chunk of retrieved document text
// blow up the prompt.
const judgeSpanFieldLimit = 500

func truncateForJudge(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= judgeSpanFieldLimit {
		return s
	}
	return s[:judgeSpanFieldLimit] + "...(截断)"
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
