package workflow

import (
	"encoding/json"
	"errors"
	"testing"
)

// Characterization tests for 链路 5 的两个纯逻辑节点：模板变量渲染和
// 条件分支求值。它们决定了节点间数据怎么流动、分支往哪走——改坏了
// 不影响编译，只有执行到对应节点才暴露。

func TestRenderTemplate(t *testing.T) {
	ec := execContext{
		Input: "用户输入",
		Prev:  "上一步输出",
		Steps: map[string]string{"s1": "步骤一结果"},
	}

	got, err := renderTemplate("{{.Input}}|{{.Prev}}|{{.Steps.s1}}", ec)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	if want := "用户输入|上一步输出|步骤一结果"; got != want {
		t.Fatalf("renderTemplate = %q, want %q", got, want)
	}
}

func TestRenderTemplateNoHTMLEscaping(t *testing.T) {
	// text/template 而非 html/template：输出是喂给 LLM 的纯文本，
	// 特殊字符绝不能被转义成 &lt; 之类。
	got, err := renderTemplate("{{.Input}}", execContext{Input: `<a href="x">&`})
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	if want := `<a href="x">&`; got != want {
		t.Fatalf("special chars were escaped: got %q, want %q", got, want)
	}
}

func TestRenderTemplateParseError(t *testing.T) {
	if _, err := renderTemplate("{{.Input", execContext{}); err == nil {
		t.Fatal("malformed template accepted")
	}
}

func condStep(config string) Step {
	return Step{ID: "cond", Type: StepConditional, Config: json.RawMessage(config)}
}

func TestRunConditional(t *testing.T) {
	svc := &service{}
	ec := execContext{Input: "yes", Prev: "prev-out", Steps: map[string]string{"s1": "42"}}

	cases := []struct {
		name       string
		expression string
		wantOutput string
	}{
		{"true branch on input", `input == "yes"`, "true"},
		{"false branch on input", `input == "no"`, "false"},
		{"env exposes prev", `prev == "prev-out"`, "true"},
		{"env exposes steps map", `steps.s1 == "42"`, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := svc.runConditional(condStep(`{"expression":`+jsonQuote(tc.expression)+`}`), ec)
			if err != nil {
				t.Fatalf("runConditional: %v", err)
			}
			if res.Output != tc.wantOutput {
				t.Fatalf("Output = %q, want %q", res.Output, tc.wantOutput)
			}
			// Input 记录的是表达式本身，供执行轨迹回放。
			if res.Input != tc.expression {
				t.Fatalf("Input = %q, want expression %q", res.Input, tc.expression)
			}
		})
	}
}

func TestRunConditionalErrors(t *testing.T) {
	svc := &service{}
	cases := []struct {
		name   string
		config string
		want   error
	}{
		{"empty config", "", ErrInvalidStepConfig},
		{"malformed json", `{not-json`, ErrInvalidStepConfig},
		{"missing expression", `{}`, ErrInvalidStepConfig},
		{"expression references unknown var", `{"expression":"nosuchvar > 1"}`, ErrInvalidStepConfig},
		{"expression not bool", `{"expression":"1 + 1"}`, ErrConditionNotBool},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.runConditional(condStep(tc.config), execContext{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("runConditional err = %v, want %v", err, tc.want)
			}
		})
	}
}

// jsonQuote JSON-quotes an expression string for embedding into step config.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
