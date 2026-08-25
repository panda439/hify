package conversation

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"hify/internal/agent"
	"hify/internal/knowledge"
	"hify/internal/platform/trace"
	"hify/internal/provider"
	"hify/internal/testutil"
)

// 001-rag-query-rerank US1 的纯函数单测：shouldSkipRewrite/parseRewriteResult/
// validateRewrite 全部零依赖（不碰网络、不碰数据库），对应宪法第 V 条"判定
// 逻辑必须可以抽成不依赖数据库的纯函数并被单元测试覆盖"的硬要求。这三个
// 函数此时尚未实现（queryrewrite.go 还不存在）——先写、先跑失败，再实现。

func TestShouldSkipRewrite(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		hasHistory bool
		want       bool
	}{
		{name: "无历史+无指代词+长度达标 -> skip", query: "Hify 的分块策略是什么", hasHistory: false, want: true},
		{name: "含它 -> 不skip", query: "那它的上限呢", hasHistory: false, want: false},
		{name: "含这个 -> 不skip", query: "这个怎么配置", hasHistory: false, want: false},
		{name: "含那个 -> 不skip", query: "那个上限是多少", hasHistory: false, want: false},
		{name: "含上述 -> 不skip", query: "上述策略适用于所有文档吗", hasHistory: false, want: false},
		{name: "含前者 -> 不skip", query: "前者的性能更好吗", hasHistory: false, want: false},
		{name: "含后者 -> 不skip", query: "后者是什么意思", hasHistory: false, want: false},
		{name: "含这些 -> 不skip", query: "这些配置在哪里改", hasHistory: false, want: false},

		// Review 修正：单字指代词「该/其/这/那」已从模式集合里删除。中文没有
		// 词边界，strings.Contains 对单字必然假阳性，而这些句子全是首轮就完整、
		// 根本不需要改写的问题——误判一次就白付一次 LLM 调用，直接打在 SC-006
		// 的「完整问题快速路径命中率 ≥90%」上。这组用例就是防它回潮的。
		{name: "假阳性防回归：应该 不含指代 -> skip", query: "我应该怎么配置 Hify 的分块大小", hasHistory: false, want: true},
		{name: "假阳性防回归：其他 不含指代 -> skip", query: "Hify 和其他 RAG 框架的区别是什么", hasHistory: false, want: true},
		{name: "假阳性防回归：其它 不含指代 -> skip", query: "除了分块还有其它优化手段吗", hasHistory: false, want: true},
		{name: "假阳性防回归：尤其 不含指代 -> skip", query: "尤其是大文档的处理流程是怎样的", hasHistory: false, want: true},
		{name: "假阳性防回归：这里 不含指代 -> skip", query: "文档上传失败时这里会记录什么日志", hasHistory: false, want: true},
		{name: "假阳性防回归：那么 不含指代 -> skip", query: "如果分块过大那么检索质量会下降吗", hasHistory: false, want: true},
		{name: "无指代词但有历史 -> 不skip", query: "Hify 的分块策略是什么", hasHistory: true, want: false},
		{name: "含指代词且有历史 -> 不skip", query: "那它呢", hasHistory: true, want: false},
		{name: "空串 -> skip", query: "", hasHistory: false, want: true},
		{name: "纯标点 -> skip", query: "？！。", hasHistory: false, want: true},
		{name: "空串但有历史 -> 仍skip（没有可改写的内容）", query: "   ", hasHistory: true, want: true},
		{name: "英文 it -> 不skip", query: "what about it", hasHistory: false, want: false},
		{name: "英文 this -> 不skip", query: "how does this work", hasHistory: false, want: false},
		{name: "英文 those -> 不skip", query: "what about those limits", hasHistory: false, want: false},
		{name: "英文完整问题无指代词 -> skip", query: "what is the chunk size limit for Hify documents", hasHistory: false, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldSkipRewrite(tc.query, tc.hasHistory)
			if got != tc.want {
				t.Errorf("shouldSkipRewrite(%q, %v) = %v, want %v", tc.query, tc.hasHistory, got, tc.want)
			}
		})
	}
}

func TestParseRewriteResult(t *testing.T) {
	t.Run("裸JSON", func(t *testing.T) {
		got, err := parseRewriteResult(`{"standalone_question":"Hify 文档分块策略的上限是多少","ambiguous":false}`)
		if err != nil {
			t.Fatalf("parseRewriteResult: %v", err)
		}
		if got.StandaloneQuestion != "Hify 文档分块策略的上限是多少" || got.Ambiguous != false {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("带json围栏", func(t *testing.T) {
		raw := "```json\n{\"standalone_question\":\"改写后的问题\",\"ambiguous\":false}\n```"
		got, err := parseRewriteResult(raw)
		if err != nil {
			t.Fatalf("parseRewriteResult: %v", err)
		}
		if got.StandaloneQuestion != "改写后的问题" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("首尾空白", func(t *testing.T) {
		raw := "  \n\t {\"standalone_question\":\"问题\",\"ambiguous\":true} \n  "
		got, err := parseRewriteResult(raw)
		if err != nil {
			t.Fatalf("parseRewriteResult: %v", err)
		}
		if !got.Ambiguous {
			t.Fatalf("got %+v, want ambiguous=true", got)
		}
	})

	t.Run("非法JSON返回error", func(t *testing.T) {
		_, err := parseRewriteResult("这不是 JSON，模型直接开始回答问题了")
		if err == nil {
			t.Fatal("expected error for unparsable output, got nil")
		}
	})

	t.Run("缺字段返回零值不报错", func(t *testing.T) {
		got, err := parseRewriteResult(`{}`)
		if err != nil {
			t.Fatalf("parseRewriteResult: %v", err)
		}
		if got.StandaloneQuestion != "" || got.Ambiguous != false {
			t.Fatalf("got %+v, want zero value", got)
		}
	})
}

func TestValidateRewrite(t *testing.T) {
	t.Run("ambiguous=true不采用", func(t *testing.T) {
		_, ok := validateRewrite("那它的上限呢", rewriteResponse{StandaloneQuestion: "随便写的", Ambiguous: true})
		if ok {
			t.Fatal("ambiguous=true must not be accepted")
		}
	})

	t.Run("空字符串不采用", func(t *testing.T) {
		_, ok := validateRewrite("那它的上限呢", rewriteResponse{StandaloneQuestion: "", Ambiguous: false})
		if ok {
			t.Fatal("empty standalone_question must not be accepted")
		}
	})

	t.Run("纯空白不采用", func(t *testing.T) {
		_, ok := validateRewrite("那它的上限呢", rewriteResponse{StandaloneQuestion: "   \n\t ", Ambiguous: false})
		if ok {
			t.Fatal("whitespace-only standalone_question must not be accepted")
		}
	})

	t.Run("超过200 runes不采用", func(t *testing.T) {
		long := make([]rune, 201)
		for i := range long {
			long[i] = '字'
		}
		_, ok := validateRewrite("这是一个不算短的原始问题示例内容", rewriteResponse{StandaloneQuestion: string(long), Ambiguous: false})
		if ok {
			t.Fatal("standalone_question over 200 runes must not be accepted")
		}
	})

	t.Run("超过max(3×原长,60)不采用", func(t *testing.T) {
		// 原问题很短（5 个字），3×原长=15，但下限是 60，所以阈值是 60；
		// 构造一个 61 rune 的候选，必须被拒绝（既没超 200，也没超 3×原长
		// 本身太小的边界，专门测 max(...) 里 60 这条下限分支）。
		original := "分块策略" // 4 runes, 3x=12, max(12,60)=60
		candidate := make([]rune, 61)
		for i := range candidate {
			candidate[i] = '问'
		}
		_, ok := validateRewrite(original, rewriteResponse{StandaloneQuestion: string(candidate), Ambiguous: false})
		if ok {
			t.Fatal("standalone_question over max(3x original, 60) runes must not be accepted")
		}
	})

	t.Run("超过3×原长（原问题较长时）不采用", func(t *testing.T) {
		// 原问题 30 runes，3×原长=90 > 60，用 91 runes 候选触发这条分支
		// （而不是 60 这条下限）。
		original := make([]rune, 30)
		for i := range original {
			original[i] = '原'
		}
		candidate := make([]rune, 91)
		for i := range candidate {
			candidate[i] = '答'
		}
		_, ok := validateRewrite(string(original), rewriteResponse{StandaloneQuestion: string(candidate), Ambiguous: false})
		if ok {
			t.Fatal("standalone_question over 3x a long original must not be accepted")
		}
	})

	// FR-005 的第三项校验「与原问题的最小相关性」——review 时补上的，之前
	// contracts §5 漏写了这一条，实现也就跟着没做。
	t.Run("与原问题完全无关不采用", func(t *testing.T) {
		_, ok := validateRewrite("Hify 的分块策略上限是多少", rewriteResponse{
			StandaloneQuestion: "北京今天的天气怎么样",
		})
		if ok {
			t.Fatal("a rewrite sharing no content signal with the original must not be accepted")
		}
	})

	t.Run("原问题只剩指代词时放行（fail open）", func(t *testing.T) {
		// 「它呢」剥掉指代词和虚词后什么都不剩，无从比较——这恰恰是本功能
		// 存在的理由（省略式追问），此时拒绝会直接废掉核心场景，必须放行。
		got, ok := validateRewrite("它呢", rewriteResponse{
			StandaloneQuestion: "Hify 文档分块策略的分块大小上限是多少",
		})
		if !ok {
			t.Fatal("must fail open when the original carries no content signal")
		}
		if got != "Hify 文档分块策略的分块大小上限是多少" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("正常改写采用并去除首尾空白", func(t *testing.T) {
		got, ok := validateRewrite("那它的上限呢", rewriteResponse{StandaloneQuestion: "  Hify 文档分块策略的分块大小上限是多少  ", Ambiguous: false})
		if !ok {
			t.Fatal("a normal, well-formed rewrite must be accepted")
		}
		if got != "Hify 文档分块策略的分块大小上限是多少" {
			t.Fatalf("got %q, want trimmed content", got)
		}
	})
}

func TestSharesRelevanceSignal(t *testing.T) {
	cases := []struct {
		name      string
		original  string
		candidate string
		want      bool
	}{
		{name: "省略式追问补全后共享实词 上限", original: "那它的上限呢", candidate: "Hify 文档分块策略的分块大小上限是多少", want: true},
		{name: "完全跑题", original: "Hify 的分块策略上限是多少", candidate: "北京今天的天气怎么样", want: false},
		{name: "原问题只剩虚词时 fail open", original: "它呢", candidate: "Hify 的分块大小上限是多少", want: true},
		{name: "英文 token 共享", original: "what about the chunk limit", candidate: "what is the chunk size limit in Hify", want: true},
		{name: "英文完全跑题", original: "what about the chunk limit", candidate: "how do I reset my password", want: false},
		{name: "只共享单个虚词不算相关", original: "分块策略是什么", candidate: "登录失败是什么原因", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sharesRelevanceSignal(tc.original, tc.candidate); got != tc.want {
				t.Errorf("sharesRelevanceSignal(%q, %q) = %v, want %v", tc.original, tc.candidate, got, tc.want)
			}
		})
	}
}

// --- 001-rag-query-rerank US3：T036，隐私断言（FR-017）---
//
// 三个用例都真的用 slog.NewJSONHandler 接管 slog.Default() 捕获 rewriteQuery
// 在三条降级路径上实际写出的日志，断言里面不含问题原文、改写结果原文——不
// 是"字段数量对不对"这种弱断言，而是显式 strings.Contains 找敏感标记串，
// 找到就直接判失败。三个用例共用的 fake（rewriteAwareChatClient/
// fakeProviderSvc）是 integration_test.go 里已经定义好的，同包内直接复用。
//
// captureSlogOutput 临时把 slog.Default() 换成写向 buf 的 JSON handler，
// defer 里换回去——package 级全局状态，这些用例都不调用 t.Parallel()，同
// 一个 `go test` 进程内没有其他测试会在这段时间并发写 slog.Default()。
func captureSlogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestRewriteQueryPrivacyUnparsableOutputNeverLogsRawContent 覆盖
// queryrewrite.go parseRewriteResult 失败分支的既有设计意图——它的文档注
// 释已经写明"Deliberately NOT logging err itself (FR-017)"，这里把它变成一
// 条会真的跑起来验证的测试，而不是只靠注释自证。
func TestRewriteQueryPrivacyUnparsableOutputNeverLogsRawContent(t *testing.T) {
	const sensitiveQuestion = "SECRET_QUESTION_不是JSON会漏出来吗_ABC123"
	const sensitiveRawOutput = "SECRET_MODEL_RAW_OUTPUT_XYZ789"

	buf := captureSlogOutput(t)

	chat := &rewriteAwareChatClient{
		// 模型没有按格式要求输出 JSON，直接把敏感标记串写进了自由文本
		// 回答里——parseRewriteResult 的 json.Unmarshal 报错信息里可能会
		// 引用被解析文本里的字符片段（例如 encoding/json 的语法错误会
		// 引出触发失败的那个字符），这正是 queryrewrite.go 里"故意不记
		// err 本身"要防的泄漏面。
		chatResponse: provider.Message{Content: sensitiveRawOutput + "，这不是合法JSON，模型直接开始回答了"},
	}
	svc := &service{
		rewriteEnabled: true,
		rewriteTimeout: 1500 * time.Millisecond,
		providerSvc:    &fakeProviderSvc{client: chat},
	}
	ag := agent.Agent{ID: "ag-privacy-unparsable", ModelID: "m1"}
	model := provider.Model{ID: "m1", ProviderID: "p1", ModelName: "chat-model"}
	// 非空历史强制 shouldSkipRewrite 不走快速路径——这个测试要验证的是
	// "真的调用了 LLM 之后降级路径不泄漏"，快速路径根本不碰 LLM，没有可
	// 测的日志泄漏面。
	history := []Message{{Role: "user", Content: "上一轮的问题"}}

	outcome := svc.rewriteQuery(context.Background(), ag, model, history, sensitiveQuestion)
	if !outcome.Degraded {
		t.Fatalf("expected Degraded=true for unparsable rewrite output, got %+v", outcome)
	}

	logged := buf.String()
	if strings.Contains(logged, sensitiveQuestion) {
		t.Fatalf("rewrite privacy log leaked the original question:\n%s", logged)
	}
	if strings.Contains(logged, sensitiveRawOutput) {
		t.Fatalf("rewrite privacy log leaked the model's raw (unparsable) output:\n%s", logged)
	}
	if !strings.Contains(logged, "invalid_json") {
		t.Fatalf("expected the structural reason=invalid_json marker in log output, got:\n%s", logged)
	}
}

// TestRewriteQueryPrivacyValidationFailureNeverLogsRawContent 覆盖
// validateRewrite 拒绝分支（比如改写结果和原问题完全跑题）——这条路径记
// 的是 "duration_ms" 而不是候选文本本身。
func TestRewriteQueryPrivacyValidationFailureNeverLogsRawContent(t *testing.T) {
	// 两个标记刻意不共享任何 ASCII 词或 CJK 双字——sharesRelevanceSignal
	// 在原问题有实词信号时才会拒绝跑题的改写，共享前缀（比如都用
	// "SECRET_"）会意外触发 fail-open 分支，掩盖了本测试真正要覆盖的
	// "校验拒绝、且不泄漏"路径（之前踩过这个坑：两个标记都带 SECRET_ 前
	// 缀，被当成"共享信号"而被判定为合法改写，Applied=true 而不是期望的
	// Degraded=true）。
	const sensitiveQuestion = "校验失败也不该漏QALPHA406"
	const sensitiveRewrite = "跑题内容RBETA321"

	buf := captureSlogOutput(t)

	chat := &rewriteAwareChatClient{
		// ambiguous=false 但改写结果和原问题没有任何共享信号——命中
		// validateRewrite 的 sharesRelevanceSignal 拒绝分支。
		chatResponse: provider.Message{Content: `{"standalone_question":"` + sensitiveRewrite + `","ambiguous":false}`},
	}
	svc := &service{
		rewriteEnabled: true,
		rewriteTimeout: 1500 * time.Millisecond,
		providerSvc:    &fakeProviderSvc{client: chat},
	}
	ag := agent.Agent{ID: "ag-privacy-validation", ModelID: "m1"}
	model := provider.Model{ID: "m1", ProviderID: "p1", ModelName: "chat-model"}
	history := []Message{{Role: "user", Content: "上一轮的问题"}}

	outcome := svc.rewriteQuery(context.Background(), ag, model, history, sensitiveQuestion)
	if !outcome.Degraded {
		t.Fatalf("expected Degraded=true for a rewrite that fails validation, got %+v", outcome)
	}

	logged := buf.String()
	if strings.Contains(logged, sensitiveQuestion) {
		t.Fatalf("rewrite privacy log leaked the original question:\n%s", logged)
	}
	if strings.Contains(logged, sensitiveRewrite) {
		t.Fatalf("rewrite privacy log leaked the rejected rewrite candidate:\n%s", logged)
	}
	if !strings.Contains(logged, "duration_ms") {
		t.Fatalf("expected the structural duration_ms field in log output, got:\n%s", logged)
	}
}

// TestRewriteQueryPrivacyLLMCallErrorNeverLogsQuestionContent 覆盖
// client.Chat 直接失败（限流/网络错误）分支——这条路径确实记了 err 本身
// （"err", err），但 err 来自 provider 层，不应该携带问题原文；这里验证的
// 是"即便 err 里意外夹带了敏感串，我们也至少能发现它"这条安全网真的能跑起
// 来，而不是形同虚设。
func TestRewriteQueryPrivacyLLMCallErrorNeverLogsQuestionContent(t *testing.T) {
	const sensitiveQuestion = "SECRET_QUESTION_LLM调用失败也不该漏_GHI789"

	buf := captureSlogOutput(t)

	chat := &rewriteAwareChatClient{
		chatErr: errors.New("simulated rewrite provider failure (rate limited)"),
	}
	svc := &service{
		rewriteEnabled: true,
		rewriteTimeout: 1500 * time.Millisecond,
		providerSvc:    &fakeProviderSvc{client: chat},
	}
	ag := agent.Agent{ID: "ag-privacy-llm-err", ModelID: "m1"}
	model := provider.Model{ID: "m1", ProviderID: "p1", ModelName: "chat-model"}
	history := []Message{{Role: "user", Content: "上一轮的问题"}}

	outcome := svc.rewriteQuery(context.Background(), ag, model, history, sensitiveQuestion)
	if !outcome.Degraded {
		t.Fatalf("expected Degraded=true for a failed rewrite LLM call, got %+v", outcome)
	}

	logged := buf.String()
	if strings.Contains(logged, sensitiveQuestion) {
		t.Fatalf("rewrite privacy log leaked the original question:\n%s", logged)
	}
}

// TestIntegrationQueryRewriteSpanNeverStoresQuestionOrRewriteContent 是
// T034 新增的 query_rewrite span 本身的隐私断言——跑一轮真的改写成功
// （Applied=true，走完整 StreamMessage）之后去 trace_spans 里查那条
// kind=query_rewrite 的 span，断言它的 Attrs 里既没有用户原问题，也没有
// LLM 改写出来的独立问题原文，即便这一轮两者都确确实实存在于其他表
// （messages）里。和 TestIntegrationTraceSpansNeverStoreFullPrivateContent
// 是同一个思路，但那条用例 rewriteEnabled=false，从未真正产出过一个
// Applied=true 的改写结果——这里专门覆盖"改写真的发生了"这个更贴近生产
// 的分支。
func TestIntegrationQueryRewriteSpanNeverStoresQuestionOrRewriteContent(t *testing.T) {
	const sensitiveQuestion = "那SECRET_SPAN_ORIGINAL_QUESTION_原问题标记_111的限制呢"
	const sensitiveRewrite = "SECRET_SPAN_REWRITTEN_QUESTION_改写后问题标记_222"

	db := testutil.MySQL(t, "conversation")
	repo := NewRepository(db)
	ctx := context.Background()

	seedConversation(t, repo, "conv-rw-span-privacy", "ag-rw-span-privacy", "u1")
	seedPriorTurn(t, repo, "conv-rw-span-privacy")

	chat := &rewriteAwareChatClient{
		scriptedChatClient: scriptedChatClient{scripts: [][]provider.ChatChunk{{
			{DeltaContent: "回答内容"},
			{FinishReason: "stop"},
		}}},
		chatResponse: provider.Message{Content: `{"standalone_question":"` + sensitiveRewrite + `","ambiguous":false}`},
	}
	knowledgeSvc := &fakeKnowledgeSvc{chunks: []knowledge.RetrievedChunk{{
		Chunk: knowledge.Chunk{KnowledgeBaseID: "kb-1", DocumentID: "doc-1", Content: "无关内容"},
		Score: 0.9,
	}}}
	svc := NewService(repo,
		&fakeAgentSvc{ag: agent.Agent{ID: "ag-rw-span-privacy", ModelID: "m1", KnowledgeBaseIDs: []string{"kb-1"}}},
		&fakeProviderSvc{client: chat},
		knowledgeSvc, &fakeMCPSvc{}, trace.NewStore(db),
		true, "", 1500*time.Millisecond)

	events, err := svc.StreamMessage(ctx, "u1", "conv-rw-span-privacy", sensitiveQuestion)
	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}
	got := drainEvents(t, events)
	if got[len(got)-1].Type != EventDone {
		t.Fatalf("turn must complete normally: %v", eventTypes(got))
	}

	if len(knowledgeSvc.queries) != 1 || knowledgeSvc.queries[0] != sensitiveRewrite {
		t.Fatalf("knowledge.Retrieve queries = %v, want exactly the rewritten question (sanity check that this test actually exercised the Applied=true path)", knowledgeSvc.queries)
	}

	spans, err := trace.NewStore(db).ListByConversation(ctx, "conv-rw-span-privacy")
	if err != nil {
		t.Fatal(err)
	}
	foundSpan := false
	for _, sp := range spans {
		if sp.Kind != trace.KindQueryRewrite {
			continue
		}
		foundSpan = true
		if strings.Contains(sp.Input, sensitiveQuestion) || strings.Contains(sp.Output, sensitiveQuestion) || strings.Contains(string(sp.Attrs), sensitiveQuestion) {
			t.Fatalf("query_rewrite span leaked the original question: Input=%q Output=%q Attrs=%s", sp.Input, sp.Output, sp.Attrs)
		}
		if strings.Contains(sp.Input, sensitiveRewrite) || strings.Contains(sp.Output, sensitiveRewrite) || strings.Contains(string(sp.Attrs), sensitiveRewrite) {
			t.Fatalf("query_rewrite span leaked the rewritten question: Input=%q Output=%q Attrs=%s", sp.Input, sp.Output, sp.Attrs)
		}
		if !strings.Contains(string(sp.Attrs), "rag.rewrite.applied") {
			t.Fatalf("query_rewrite span attrs missing rag.rewrite.applied: %s", sp.Attrs)
		}
	}
	if !foundSpan {
		t.Fatal("no query_rewrite span recorded")
	}
}
