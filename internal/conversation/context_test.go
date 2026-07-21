package conversation

import (
	"strings"
	"testing"

	"hify/internal/provider"
)

// Characterization tests for 链路 1 的上下文组装预算逻辑：历史消息按
// 字符预算从旧到新截断。改坏的表现是超长会话把模型上下文撑爆（或把
// 用户刚发的消息裁掉），只有长会话才暴露。

func msgs(contents ...string) []Message {
	out := make([]Message, len(contents))
	for i, c := range contents {
		out[i] = Message{Content: c}
	}
	return out
}

func contents(rows []Message) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Content
	}
	return out
}

func TestTruncateByBudget(t *testing.T) {
	cases := []struct {
		name   string
		rows   []Message
		budget int
		want   []string
	}{
		{"empty input", nil, 100, nil},
		{"all fit", msgs("aa", "bb", "cc"), 100, []string{"aa", "bb", "cc"}},
		{"drop oldest first", msgs("aaaa", "bbbb", "cccc"), 8, []string{"bbbb", "cccc"}},
		{"exact boundary kept", msgs("aaaa", "bbbb"), 8, []string{"aaaa", "bbbb"}},
		{"zero budget keeps latest", msgs("aaaa", "bbbb"), 0, []string{"bbbb"}},
		{"negative budget keeps latest", msgs("aaaa"), -1, []string{"aaaa"}},
		{"single oversized message still kept", msgs("aaaaaaaaaa"), 3, []string{"aaaaaaaaaa"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contents(truncateByBudget(tc.rows, tc.budget))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func iptr(i int) *int { return &i }

func TestContextBudgetChars(t *testing.T) {
	cases := []struct {
		name   string
		model  provider.Model
		sysLen int
		want   int
	}{
		// 无 ContextWindow：默认 4000 token * 4 chars/token。
		{"default budget", provider.Model{}, 0, 16000},
		// 有 ContextWindow：(窗口 - 1000 输出预留) * 4。
		{"window minus output reserve", provider.Model{ContextWindow: iptr(3000)}, 0, 8000},
		// 窗口太小时钳到 500 token 下限。
		{"clamped to minimum", provider.Model{ContextWindow: iptr(1100)}, 0, 2000},
		// system prompt 长度从字符预算里扣。
		{"system prompt deducted", provider.Model{}, 500, 15500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contextBudgetChars(tc.model, strings.Repeat("x", tc.sysLen))
			if got != tc.want {
				t.Fatalf("contextBudgetChars = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFormatRetrievedContextMentionsOptional(t *testing.T) {
	// 锁住"参考资料是可选上下文而非指令"这一措辞意图和片段编号格式。
	got := formatRetrievedContext(nil)
	if !strings.Contains(got, "可以忽略") {
		t.Fatalf("preamble lost the '资料无关可忽略' framing: %q", got)
	}
}
