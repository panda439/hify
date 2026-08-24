package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"text/template"
	"time"

	"github.com/expr-lang/expr"

	"hify/internal/platform"
	"hify/internal/provider"
)

// maxWorkflowSteps bounds one Execute call the same way
// conversation.maxToolCallIterations bounds the tool-calling loop — a
// misconfigured (but structurally valid, so Validate() didn't catch it)
// definition must not be able to hang a request forever.
const maxWorkflowSteps = 50

// execContext is the template/expression environment every step's config
// templates and conditional expressions are evaluated against. Steps is
// keyed by step ID and only ever grows (never removed), so a later step
// can reference any earlier step's output by ID; Prev is a shortcut for
// "whatever the immediately preceding step produced" so a template doesn't
// need to know its own predecessor's ID.
type execContext struct {
	Input string
	Prev  string
	Steps map[string]string
}

// stepResult is what each step-type runner returns: Output is what gets
// recorded as the step's result and fed forward into later steps' context;
// Input is the fully-rendered prompt/query/arguments actually sent
// downstream, kept for the execution trace (workflow_run_steps.input) even
// when the step fails, so a debugging user can see exactly what was sent.
type stepResult struct {
	Input  string
	Output string
}

// Execute validates the workflow is runnable and inserts the run's row
// synchronously (so "not found"/"not active" surface as a normal handler
// error before any response header is committed), then hands the actual
// step loop off to a goroutine that streams progress over the returned
// channel — mirrors conversation.StreamMessage's identical shape for the
// identical reason: the HTTP handler turns each received value into an SSE
// frame via c.Stream, and once that starts, only in-band events (not a
// second HTTP response) can signal success or failure.
func (s *service) Execute(ctx context.Context, workflowID, userID, input string) (<-chan StepEvent, error) {
	wf, err := s.repo.getWorkflow(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	if !wf.IsActive {
		return nil, ErrWorkflowNotActive
	}

	run := WorkflowRun{
		ID:         platform.NewID(),
		WorkflowID: wf.ID,
		Status:     RunStatusRunning,
		Input:      input,
		CreatedBy:  userID,
	}
	if err := s.repo.createRun(ctx, run); err != nil {
		return nil, err
	}

	events := make(chan StepEvent)
	go s.runWorkflow(ctx, wf, run, input, events)
	return events, nil
}

// runWorkflow is the step-execution loop, run in its own goroutine by
// Execute. It always closes events exactly once (deferred) and always
// finalizes the run's row exactly once (success, a step failure, or
// hitting maxWorkflowSteps) — see the migration's note on workflow_runs
// being a job-status row, not an append-only log like workflow_run_steps.
// errClientDisconnected is stored as-is into run.ErrorMessage (a
// user-visible column, per GetRun/ListRuns), so its text is curated Chinese
// rather than an internal fmt.Errorf chain — see CLAUDE.md's rule that only
// apperr.Message-style strings are safe to show a client.
var errClientDisconnected = errors.New("客户端连接已断开，工作流执行已终止")

func (s *service) runWorkflow(ctx context.Context, wf Workflow, run WorkflowRun, input string, events chan<- StepEvent) {
	defer close(events)
	// Must run after (i.e. be deferred before, since defers are LIFO) the
	// close(events) above so a recovered panic can still get an error event
	// out on the still-open channel — see CLAUDE.md's goroutine recover
	// rule.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("workflow: panic in runWorkflow", "panic", r, "run_id", run.ID, "stack", string(debug.Stack()))
			trySend(ctx, events, StepEvent{Type: EventError, RunID: run.ID, Error: "服务器内部错误，请稍后重试"})
		}
	}()

	ec := execContext{Input: input, Steps: map[string]string{}}
	current, _ := wf.Definition.startStep() // Validate() on save guarantees this exists

	var finalOutput string
	var runErr error

	for i := 0; ; i++ {
		if i >= maxWorkflowSteps {
			runErr = ErrTooManySteps
			break
		}

		if !trySend(ctx, events, StepEvent{Type: EventStep, RunID: run.ID, StepID: current.ID, StepType: string(current.Type), Status: "running"}) {
			// Nobody downstream is reading anymore (client disconnected) —
			// a plain send would block this goroutine forever, and the
			// run's row would be stuck at status="running" forever since
			// the finishRun call below would never be reached. Stop
			// executing further steps and fall through to finalize the
			// run's row as failed instead.
			runErr = errClientDisconnected
			break
		}
		startedAt := time.Now()
		result, stepErr := s.runStep(ctx, current, ec, input)
		finishedAt := time.Now()

		if stepErr != nil {
			if err := s.repo.createRunStep(ctx, WorkflowRunStep{
				ID: platform.NewID(), WorkflowRunID: run.ID, StepID: current.ID,
				StepType: string(current.Type), Status: StepStatusFailed,
				Input: result.Input, ErrorMessage: stepErr.Error(),
				StartedAt: startedAt, FinishedAt: &finishedAt,
			}); err != nil {
				slog.Error("workflow: persist failed step", "err", err, "run_id", run.ID, "step_id", current.ID)
			}
			trySend(ctx, events, StepEvent{Type: EventStep, RunID: run.ID, StepID: current.ID, StepType: string(current.Type), Status: "failed", Error: stepErr.Error()})
			runErr = stepErr
			break
		}

		if err := s.repo.createRunStep(ctx, WorkflowRunStep{
			ID: platform.NewID(), WorkflowRunID: run.ID, StepID: current.ID,
			StepType: string(current.Type), Status: StepStatusSucceeded,
			Input: result.Input, Output: result.Output,
			StartedAt: startedAt, FinishedAt: &finishedAt,
		}); err != nil {
			slog.Error("workflow: persist succeeded step", "err", err, "run_id", run.ID, "step_id", current.ID)
		}
		if !trySend(ctx, events, StepEvent{Type: EventStep, RunID: run.ID, StepID: current.ID, StepType: string(current.Type), Status: "succeeded", Output: result.Output}) {
			runErr = errClientDisconnected
			break
		}

		ec.Steps[current.ID] = result.Output
		ec.Prev = result.Output

		if current.Type == StepEnd {
			finalOutput = result.Output
			break
		}

		nextID := current.Next
		if current.Type == StepConditional {
			if result.Output == "true" {
				nextID = current.NextIfTrue
			} else {
				nextID = current.NextIfFalse
			}
		}
		next, ok := wf.Definition.StepByID(nextID)
		if !ok {
			// Validate() on save guarantees every edge target exists —
			// reaching here means the persisted definition and this code
			// disagree, a bug rather than a user-input problem.
			runErr = fmt.Errorf("workflow: step %q references missing step %q", current.ID, nextID)
			break
		}
		current = next
	}

	finished := time.Now()
	run.FinishedAt = &finished
	if runErr != nil {
		run.Status = RunStatusFailed
		run.ErrorMessage = runErr.Error()
	} else {
		run.Status = RunStatusSucceeded
		run.Output = finalOutput
	}
	if err := s.repo.finishRun(ctx, run); err != nil {
		slog.Error("workflow: finish run", "err", err, "run_id", run.ID)
		trySend(ctx, events, StepEvent{Type: EventError, RunID: run.ID, Error: "工作流执行完成，但保存结果失败"})
		return
	}

	if runErr != nil {
		trySend(ctx, events, StepEvent{Type: EventError, RunID: run.ID, Error: runErr.Error()})
		return
	}
	trySend(ctx, events, StepEvent{Type: EventDone, RunID: run.ID, Output: finalOutput})
}

// trySend delivers evt on events but never blocks past ctx's cancellation —
// same rationale as conversation.trySend: once the client disconnects,
// nobody downstream reads events anymore, and a plain send would pin this
// goroutine forever.
func trySend(ctx context.Context, events chan<- StepEvent, evt StepEvent) bool {
	select {
	case events <- evt:
		return true
	case <-ctx.Done():
		return false
	}
}

// runStep dispatches to the step-type-specific runner. For a conditional
// step, Output is literally "true" or "false" — Execute's loop reads that
// to choose NextIfTrue/NextIfFalse, keeping "what a step does" separate
// from "what runs after it".
func (s *service) runStep(ctx context.Context, step Step, ec execContext, workflowInput string) (stepResult, error) {
	switch step.Type {
	case StepStart:
		return stepResult{Output: workflowInput}, nil
	case StepEnd:
		return s.runEnd(step, ec)
	case StepLLMCall:
		return s.runLLMCall(ctx, step, ec)
	case StepKnowledgeRetrieval:
		return s.runKnowledgeRetrieval(ctx, step, ec)
	case StepConditional:
		return s.runConditional(step, ec)
	case StepToolCall:
		return s.runToolCall(ctx, step, ec)
	default:
		// Unreachable given Validate()'s step-type checks at save time —
		// see dag.go.
		return stepResult{}, ErrInvalidStepConfig
	}
}

type endConfig struct {
	OutputTemplate string `json:"output_template"`
}

// runEnd defaults to passing through whatever the previous step produced;
// an explicit output_template lets the workflow author compose a final
// answer from multiple earlier steps instead.
func (s *service) runEnd(step Step, ec execContext) (stepResult, error) {
	tmplStr := "{{.Prev}}"
	if len(step.Config) > 0 {
		var cfg endConfig
		if err := json.Unmarshal(step.Config, &cfg); err != nil {
			return stepResult{}, ErrInvalidStepConfig
		}
		if cfg.OutputTemplate != "" {
			tmplStr = cfg.OutputTemplate
		}
	}
	out, err := renderTemplate(tmplStr, ec)
	if err != nil {
		return stepResult{}, ErrInvalidStepConfig
	}
	return stepResult{Output: out}, nil
}

type llmCallConfig struct {
	ModelID        string  `json:"model_id"`
	SystemPrompt   string  `json:"system_prompt"`
	PromptTemplate string  `json:"prompt_template"`
	Temperature    float64 `json:"temperature"`
	MaxTokens      int     `json:"max_tokens"`
}

func (s *service) runLLMCall(ctx context.Context, step Step, ec execContext) (stepResult, error) {
	if len(step.Config) == 0 {
		return stepResult{}, ErrInvalidStepConfig
	}
	var cfg llmCallConfig
	if err := json.Unmarshal(step.Config, &cfg); err != nil || cfg.ModelID == "" || cfg.PromptTemplate == "" {
		return stepResult{}, ErrInvalidStepConfig
	}
	prompt, err := renderTemplate(cfg.PromptTemplate, ec)
	if err != nil {
		return stepResult{}, ErrInvalidStepConfig
	}

	model, err := s.providerSvc.GetModel(ctx, cfg.ModelID)
	if err != nil {
		return stepResult{Input: prompt}, err
	}
	client, err := s.providerSvc.ResolveClient(ctx, model.ProviderID)
	if err != nil {
		return stepResult{Input: prompt}, err
	}

	var messages []provider.Message
	if cfg.SystemPrompt != "" {
		sysPrompt, err := renderTemplate(cfg.SystemPrompt, ec)
		if err != nil {
			return stepResult{Input: prompt}, ErrInvalidStepConfig
		}
		messages = append(messages, provider.Message{Role: provider.RoleSystem, Content: sysPrompt})
	}
	messages = append(messages, provider.Message{Role: provider.RoleUser, Content: prompt})

	temperature := cfg.Temperature
	if temperature == 0 {
		temperature = 0.7
	}

	reply, err := client.Chat(ctx, provider.ChatRequest{
		Model:    model.ModelName,
		Messages: messages,
		// temperature 在上面已经被规范化为"非零"（0 会被视为未配置->0.7 默
		// 认值，workflow 自己既有的语义，本次不改），所以这里恒是显式非
		// 零值，取地址即可——不涉及 T023a 修的"显式 0 被吞"那个坑。
		Temperature: &temperature,
		MaxTokens:   cfg.MaxTokens,
	})
	if err != nil {
		return stepResult{Input: prompt}, fmt.Errorf("workflow: llm_call: %w", err)
	}
	return stepResult{Input: prompt, Output: reply.Content}, nil
}

type knowledgeRetrievalConfig struct {
	KnowledgeBaseIDs []string `json:"knowledge_base_ids"`
	QueryTemplate    string   `json:"query_template"`
	TopK             int      `json:"top_k"`
}

func (s *service) runKnowledgeRetrieval(ctx context.Context, step Step, ec execContext) (stepResult, error) {
	if len(step.Config) == 0 {
		return stepResult{}, ErrInvalidStepConfig
	}
	var cfg knowledgeRetrievalConfig
	if err := json.Unmarshal(step.Config, &cfg); err != nil || len(cfg.KnowledgeBaseIDs) == 0 || cfg.QueryTemplate == "" {
		return stepResult{}, ErrInvalidStepConfig
	}
	query, err := renderTemplate(cfg.QueryTemplate, ec)
	if err != nil {
		return stepResult{}, ErrInvalidStepConfig
	}
	topK := cfg.TopK
	if topK <= 0 {
		topK = 5
	}

	chunks, err := s.knowledgeSvc.Retrieve(ctx, cfg.KnowledgeBaseIDs, query, topK)
	if err != nil {
		return stepResult{Input: query}, fmt.Errorf("workflow: knowledge_retrieval: %w", err)
	}

	var buf strings.Builder
	for i, c := range chunks {
		if i > 0 {
			buf.WriteString("\n\n")
		}
		buf.WriteString(c.Content)
	}
	return stepResult{Input: query, Output: buf.String()}, nil
}

type conditionalConfig struct {
	Expression string `json:"expression"`
}

func (s *service) runConditional(step Step, ec execContext) (stepResult, error) {
	if len(step.Config) == 0 {
		return stepResult{}, ErrInvalidStepConfig
	}
	var cfg conditionalConfig
	if err := json.Unmarshal(step.Config, &cfg); err != nil || cfg.Expression == "" {
		return stepResult{}, ErrInvalidStepConfig
	}

	env := map[string]any{"input": ec.Input, "prev": ec.Prev, "steps": ec.Steps}
	result, err := expr.Eval(cfg.Expression, env)
	if err != nil {
		return stepResult{Input: cfg.Expression}, ErrInvalidStepConfig
	}
	b, ok := result.(bool)
	if !ok {
		return stepResult{Input: cfg.Expression}, ErrConditionNotBool
	}
	if b {
		return stepResult{Input: cfg.Expression, Output: "true"}, nil
	}
	return stepResult{Input: cfg.Expression, Output: "false"}, nil
}

type toolCallConfig struct {
	MCPToolID         string `json:"mcp_tool_id"`
	ArgumentsTemplate string `json:"arguments_template"`
}

func (s *service) runToolCall(ctx context.Context, step Step, ec execContext) (stepResult, error) {
	if len(step.Config) == 0 {
		return stepResult{}, ErrInvalidStepConfig
	}
	var cfg toolCallConfig
	if err := json.Unmarshal(step.Config, &cfg); err != nil || cfg.MCPToolID == "" {
		return stepResult{}, ErrInvalidStepConfig
	}

	argsStr := "{}"
	if cfg.ArgumentsTemplate != "" {
		rendered, err := renderTemplate(cfg.ArgumentsTemplate, ec)
		if err != nil {
			return stepResult{}, ErrInvalidStepConfig
		}
		argsStr = rendered
	}
	if !json.Valid([]byte(argsStr)) {
		return stepResult{Input: argsStr}, ErrInvalidStepConfig
	}

	result, err := s.mcpSvc.CallTool(ctx, cfg.MCPToolID, json.RawMessage(argsStr))
	if err != nil {
		return stepResult{Input: argsStr}, fmt.Errorf("workflow: tool_call: %w", err)
	}
	if result.IsError {
		return stepResult{Input: argsStr, Output: result.Content}, ErrToolCallFailed
	}
	return stepResult{Input: argsStr, Output: result.Content}, nil
}

// renderTemplate uses Go's text/template (not html/template — step output
// is plain text fed to an LLM prompt or a JSON arguments blob, never
// rendered into HTML) so step configs can reference {{.Input}}, {{.Prev}},
// and {{.Steps.<id>}}.
func renderTemplate(tmplStr string, ec execContext) (string, error) {
	tmpl, err := template.New("workflow-step").Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("workflow: parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ec); err != nil {
		return "", fmt.Errorf("workflow: render template: %w", err)
	}
	return buf.String(), nil
}
