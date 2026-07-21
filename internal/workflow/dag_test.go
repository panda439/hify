package workflow

import (
	"errors"
	"testing"
)

// Characterization tests for Definition.Validate — 锁住链路 5（工作流执行）
// 的入口护栏行为：非法 DAG 必须在保存/执行前被拒绝，executor 依赖这里
// 校验过的不变量在运行时不再重查。

func linear(ids ...string) Definition {
	// ids[0] 为 start，最后一个为 end，中间全是 llm_call，首尾相连。
	steps := make([]Step, len(ids))
	for i, id := range ids {
		s := Step{ID: id, Type: StepLLMCall}
		if i == 0 {
			s.Type = StepStart
		}
		if i == len(ids)-1 {
			s.Type = StepEnd
		} else {
			s.Next = ids[i+1]
		}
		steps[i] = s
	}
	return Definition{Steps: steps}
}

func TestValidateLinearOK(t *testing.T) {
	if err := linear("start", "a", "end").Validate(); err != nil {
		t.Fatalf("valid linear DAG rejected: %v", err)
	}
}

func TestValidateConditionalOK(t *testing.T) {
	d := Definition{Steps: []Step{
		{ID: "start", Type: StepStart, Next: "cond"},
		{ID: "cond", Type: StepConditional, NextIfTrue: "a", NextIfFalse: "b"},
		{ID: "a", Type: StepLLMCall, Next: "end"},
		{ID: "b", Type: StepKnowledgeRetrieval, Next: "end"},
		{ID: "end", Type: StepEnd},
	}}
	if err := d.Validate(); err != nil {
		t.Fatalf("valid conditional DAG rejected: %v", err)
	}
}

func TestValidateRejections(t *testing.T) {
	cases := []struct {
		name string
		def  Definition
		want error
	}{
		{"empty definition", Definition{}, ErrEmptyDefinition},
		{"blank step id", Definition{Steps: []Step{{ID: "", Type: StepStart}}}, ErrInvalidStepConfig},
		{"duplicate step id", Definition{Steps: []Step{
			{ID: "x", Type: StepStart, Next: "x"},
			{ID: "x", Type: StepEnd},
		}}, ErrDuplicateStepID},
		{"no start step", Definition{Steps: []Step{
			{ID: "end", Type: StepEnd},
		}}, ErrMissingStartStep},
		{"two start steps", Definition{Steps: []Step{
			{ID: "s1", Type: StepStart, Next: "end"},
			{ID: "s2", Type: StepStart, Next: "end"},
			{ID: "end", Type: StepEnd},
		}}, ErrMissingStartStep},
		{"dangling edge", Definition{Steps: []Step{
			{ID: "start", Type: StepStart, Next: "ghost"},
		}}, ErrDanglingEdge},
		{"conditional missing false branch", Definition{Steps: []Step{
			{ID: "start", Type: StepStart, Next: "cond"},
			{ID: "cond", Type: StepConditional, NextIfTrue: "end"},
			{ID: "end", Type: StepEnd},
		}}, ErrConditionalMissingBranch},
		{"non-end step with no outgoing edge", Definition{Steps: []Step{
			{ID: "start", Type: StepStart, Next: "a"},
			{ID: "a", Type: StepLLMCall},
		}}, ErrDeadEnd},
		{"end step with outgoing edge", Definition{Steps: []Step{
			{ID: "start", Type: StepStart, Next: "end"},
			{ID: "end", Type: StepEnd, Next: "start"},
		}}, ErrEndHasOutgoing},
		{"cycle", Definition{Steps: []Step{
			{ID: "start", Type: StepStart, Next: "a"},
			{ID: "a", Type: StepLLMCall, Next: "b"},
			{ID: "b", Type: StepLLMCall, Next: "a"},
		}}, ErrCycleDetected},
		{"unreachable step", Definition{Steps: []Step{
			{ID: "start", Type: StepStart, Next: "end"},
			{ID: "end", Type: StepEnd},
			{ID: "orphan", Type: StepLLMCall, Next: "end"},
		}}, ErrUnreachableStep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.def.Validate()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}
