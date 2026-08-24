package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"hify/internal/knowledge"
	"hify/internal/mcp"
	"hify/internal/provider"
	"hify/internal/testutil"
)

// 链路 5（工作流执行）的集成测试：真实 MySQL 存 workflow/run/step 轨迹，
// provider/knowledge/mcp 走 Service 接口 fake。验证的是执行器编排 + 轨迹
// 持久化这两件事的合同：分支走向正确、每个节点留下可回放的 step 行、
// run 状态机收敛（succeeded/failed，绝不卡 running）。

type wfFakeProvider struct {
	provider.Service
	chatContent string
	prompts     []string
}

func (f *wfFakeProvider) GetModel(ctx context.Context, id string) (provider.Model, error) {
	if id != "m1" {
		return provider.Model{}, provider.ErrModelNotFound
	}
	return provider.Model{ID: id, ProviderID: "p1", ModelName: "chat-model", Capability: provider.CapabilityChat}, nil
}

func (f *wfFakeProvider) ResolveClient(ctx context.Context, providerID string) (provider.Client, error) {
	return &wfFakeChatClient{svc: f}, nil
}

type wfFakeChatClient struct {
	provider.Client
	svc *wfFakeProvider
}

func (c *wfFakeChatClient) Chat(ctx context.Context, req provider.ChatRequest) (provider.Message, error) {
	// 记录最终发给 LLM 的 prompt（最后一条 user 消息），供模板渲染断言。
	c.svc.prompts = append(c.svc.prompts, req.Messages[len(req.Messages)-1].Content)
	return provider.Message{Role: provider.RoleAssistant, Content: c.svc.chatContent}, nil
}

// Rerank：001-rag-query-rerank（T024）给 provider.Client 接口新增的方法，
// 补一个空实现，workflow 链路本身不涉及 rerank。
func (c *wfFakeChatClient) Rerank(ctx context.Context, req provider.RerankRequest) (provider.RerankResult, error) {
	return provider.RerankResult{}, nil
}

type wfFakeKnowledge struct{ knowledge.Service }

type wfFakeMCP struct{ mcp.Service }

func newWFService(t *testing.T) (Service, *Repository, *wfFakeProvider) {
	t.Helper()
	repo := NewRepository(testutil.MySQL(t, "workflow"))
	fp := &wfFakeProvider{chatContent: "LLM回复"}
	return NewService(repo, fp, &wfFakeKnowledge{}, &wfFakeMCP{}), repo, fp
}

// condDefinition: start → cond(input=="yes") → true: llm → end
//                                            → false: end2(固定文案)
func condDefinition() Definition {
	return Definition{Steps: []Step{
		{ID: "start", Type: StepStart, Next: "cond"},
		{ID: "cond", Type: StepConditional, NextIfTrue: "llm", NextIfFalse: "end2",
			Config: json.RawMessage(`{"expression":"input == \"yes\""}`)},
		{ID: "llm", Type: StepLLMCall, Next: "end",
			Config: json.RawMessage(`{"model_id":"m1","prompt_template":"请回答：{{.Input}}"}`)},
		{ID: "end", Type: StepEnd},
		{ID: "end2", Type: StepEnd, Config: json.RawMessage(`{"output_template":"已拒绝"}`)},
	}}
}

func drainSteps(t *testing.T, events <-chan StepEvent) []StepEvent {
	t.Helper()
	var out []StepEvent
	for e := range events {
		out = append(out, e)
	}
	return out
}

func succeededStepIDs(events []StepEvent) []string {
	var out []string
	for _, e := range events {
		if e.Type == EventStep && e.Status == "succeeded" {
			out = append(out, e.StepID)
		}
	}
	return out
}

func TestIntegrationExecuteTrueBranch(t *testing.T) {
	svc, repo, fp := newWFService(t)
	ctx := context.Background()

	wf, err := svc.CreateWorkflow(ctx, CreateWorkflowInput{
		Name: "分支流", Definition: condDefinition(), CreatedBy: "u1",
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	events, err := svc.Execute(ctx, wf.ID, "u1", "yes")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := drainSteps(t, events)

	// 走 true 分支：start → cond → llm → end；终帧是 done 且输出为 LLM 回复
	//（end 默认透传 Prev）。
	path := succeededStepIDs(got)
	if strings.Join(path, ",") != "start,cond,llm,end" {
		t.Fatalf("execution path = %v", path)
	}
	last := got[len(got)-1]
	if last.Type != EventDone || last.Output != "LLM回复" {
		t.Fatalf("final event = %+v, want done/LLM回复", last)
	}
	// 模板渲染进了真实发给 LLM 的 prompt。
	if len(fp.prompts) != 1 || fp.prompts[0] != "请回答：yes" {
		t.Fatalf("rendered prompt = %v", fp.prompts)
	}

	// DB 轨迹和 SSE 事件一致，且可回放（llm 步的 input 是渲染后的 prompt）。
	run, err := repo.getRun(ctx, last.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunStatusSucceeded || run.Output != "LLM回复" || run.FinishedAt == nil {
		t.Fatalf("run row = %+v", run)
	}
	steps, err := repo.listRunSteps(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 4 {
		t.Fatalf("persisted %d steps, want 4", len(steps))
	}
	byID := map[string]WorkflowRunStep{}
	for _, s := range steps {
		byID[s.StepID] = s
	}
	if byID["cond"].Output != "true" {
		t.Fatalf("cond step output = %q, want true", byID["cond"].Output)
	}
	if byID["llm"].Input != "请回答：yes" || byID["llm"].Output != "LLM回复" {
		t.Fatalf("llm step trace = %+v", byID["llm"])
	}
}

func TestIntegrationExecuteFalseBranch(t *testing.T) {
	svc, repo, _ := newWFService(t)
	ctx := context.Background()

	wf, err := svc.CreateWorkflow(ctx, CreateWorkflowInput{
		Name: "分支流-false", Definition: condDefinition(), CreatedBy: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := svc.Execute(ctx, wf.ID, "u1", "no")
	if err != nil {
		t.Fatal(err)
	}
	got := drainSteps(t, events)

	path := succeededStepIDs(got)
	if strings.Join(path, ",") != "start,cond,end2" {
		t.Fatalf("execution path = %v, want false branch", path)
	}
	last := got[len(got)-1]
	if last.Type != EventDone || last.Output != "已拒绝" {
		t.Fatalf("final event = %+v, want done/已拒绝", last)
	}
	run, _ := repo.getRun(ctx, last.RunID)
	if run.Status != RunStatusSucceeded || run.Output != "已拒绝" {
		t.Fatalf("run row = %+v", run)
	}
}

func TestIntegrationExecuteStepFailureFinalizesRun(t *testing.T) {
	// llm 步引用不存在的模型：run 必须收敛到 failed（不卡 running），
	// 失败的 step 行也要留轨迹。
	svc, repo, _ := newWFService(t)
	ctx := context.Background()

	def := Definition{Steps: []Step{
		{ID: "start", Type: StepStart, Next: "llm"},
		{ID: "llm", Type: StepLLMCall, Next: "end",
			Config: json.RawMessage(`{"model_id":"ghost","prompt_template":"x"}`)},
		{ID: "end", Type: StepEnd},
	}}
	wf, err := svc.CreateWorkflow(ctx, CreateWorkflowInput{Name: "坏模型", Definition: def, CreatedBy: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := svc.Execute(ctx, wf.ID, "u1", "in")
	if err != nil {
		t.Fatal(err)
	}
	got := drainSteps(t, events)
	last := got[len(got)-1]
	if last.Type != EventError {
		t.Fatalf("final event = %+v, want error", last)
	}
	run, err := repo.getRun(ctx, last.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunStatusFailed || run.ErrorMessage == "" || run.FinishedAt == nil {
		t.Fatalf("run must finalize as failed with message: %+v", run)
	}
	steps, _ := repo.listRunSteps(ctx, run.ID)
	var failed *WorkflowRunStep
	for i := range steps {
		if steps[i].Status == StepStatusFailed {
			failed = &steps[i]
		}
	}
	if failed == nil || failed.StepID != "llm" {
		t.Fatalf("failed llm step not in trace: %+v", steps)
	}
}

func TestIntegrationCreateWorkflowRejectsInvalidDAG(t *testing.T) {
	svc, _, _ := newWFService(t)
	// 带环的定义在保存时就要被拒（链路 5 终点标准：非法 DAG 不落库）。
	def := Definition{Steps: []Step{
		{ID: "start", Type: StepStart, Next: "a"},
		{ID: "a", Type: StepLLMCall, Next: "start"},
	}}
	_, err := svc.CreateWorkflow(context.Background(), CreateWorkflowInput{Name: "环", Definition: def, CreatedBy: "u1"})
	if !errors.Is(err, ErrCycleDetected) {
		t.Fatalf("CreateWorkflow err = %v, want ErrCycleDetected", err)
	}
}

func TestIntegrationExecuteInactiveWorkflowRejected(t *testing.T) {
	svc, repo, _ := newWFService(t)
	ctx := context.Background()

	wf, err := svc.CreateWorkflow(ctx, CreateWorkflowInput{Name: "停用流", Definition: condDefinition(), CreatedBy: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	wf.IsActive = false
	if err := repo.updateWorkflow(ctx, wf); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(ctx, wf.ID, "u1", "yes"); !errors.Is(err, ErrWorkflowNotActive) {
		t.Fatalf("Execute on inactive workflow err = %v, want ErrWorkflowNotActive", err)
	}
}
