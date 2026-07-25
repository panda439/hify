package conversation

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"hify/internal/agent"
	"hify/internal/knowledge"
	"hify/internal/platform"
	"hify/internal/platform/trace"
	"hify/internal/provider"
	"hify/internal/testutil"
)

// --- 第一轮代码审查修复：问题三（预算与实际发送内容一致） ---
//
// 这些测试直接调用 (*service).assembleContext（白盒，同包），比
// selectEvidence 的纯函数单测更进一步：验证的是"一整轮真正组装出来的
// provider.Message 列表"在各种 evidence 场景下是否真的没有被幽灵预算
// 扣掉不该扣的历史消息。

func newAssembleTestService(db *sql.DB, knowledgeSvc knowledge.Service) *service {
	return &service{repo: NewRepository(db), knowledgeSvc: knowledgeSvc, traceStore: trace.NewStore(db)}
}

// seedHistory inserts n user/assistant-alternating messages of the given
// content, then a final user message with latestContent — the row
// assembleContext expects to already exist (StreamMessage always persists
// the incoming user message before calling assembleContext; these tests
// reproduce that precondition by hand since they call assembleContext
// directly).
func seedHistory(t *testing.T, repo *Repository, conversationID string, n int, contentEach, latestContent string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if err := repo.createMessage(ctx, Message{ID: platform.NewID(), ConversationID: conversationID, Role: role, Content: contentEach}); err != nil {
			t.Fatalf("seed history message %d: %v", i, err)
		}
	}
	if err := repo.createMessage(ctx, Message{ID: platform.NewID(), ConversationID: conversationID, Role: "user", Content: latestContent}); err != nil {
		t.Fatalf("seed latest message: %v", err)
	}
}

// smallWindowModel gives a small, deterministic total budget: (1500-1000)
// tokens * 4 chars/token = 2000 chars — small enough that a phantom
// RAG/citation-rules reservation (the old bug) would visibly starve
// history, but large enough to comfortably hold a handful of short
// messages when nothing is (wrongly) reserved.
var smallWindowModel = provider.Model{ContextWindow: intp(1500)}

func TestIntegrationAssembleContextNoKnowledgeBasesChargesNothingForRAG(t *testing.T) {
	db := testutil.MySQL(t, "conversation")
	repo := NewRepository(db)
	svc := &service{repo: repo, traceStore: trace.NewStore(db)}

	seedConversation(t, repo, "conv-nokb", "ag-nokb", "u1")
	seedHistory(t, repo, "conv-nokb", 3, strings.Repeat("h", 100), "最新问题")

	ag := agent.Agent{ID: "ag-nokb", ModelID: "m1"} // no KnowledgeBaseIDs
	assembled, err := svc.assembleContext(context.Background(), "conv-nokb", ag, smallWindowModel, "最新问题", "trace-1")
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	// 4 条历史(3条填充+1条最新) 全部保留——如果 RAG/citation 被幽灵扣费
	// （旧 bug），2000 字符预算会被 2000-token 的 RAG 保留吃光，只剩最新
	// 一条。
	if len(assembled.Messages) != 4 {
		t.Fatalf("assembled %d messages, want all 4 history rows kept (no RAG reservation without knowledge bases): %+v", len(assembled.Messages), assembled.Messages)
	}
	if len(assembled.Evidence) != 0 {
		t.Fatalf("evidence = %+v, want none (no knowledge bases attached)", assembled.Evidence)
	}
}

func TestIntegrationAssembleContextRetrievalErrorChargesNothingForRAG(t *testing.T) {
	db := testutil.MySQL(t, "conversation")
	repo := NewRepository(db)
	svc := newAssembleTestService(db, &fakeKnowledgeSvc{err: errFakeRetrieval})

	seedConversation(t, repo, "conv-kberr", "ag-kberr", "u1")
	seedHistory(t, repo, "conv-kberr", 3, strings.Repeat("h", 100), "最新问题")

	ag := agent.Agent{ID: "ag-kberr", ModelID: "m1", KnowledgeBaseIDs: []string{"kb-1"}}
	assembled, err := svc.assembleContext(context.Background(), "conv-kberr", ag, smallWindowModel, "最新问题", "trace-1")
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	if len(assembled.Messages) != 4 {
		t.Fatalf("assembled %d messages, want all 4 history rows kept (retrieval failure must not reserve RAG budget): %+v", len(assembled.Messages), assembled.Messages)
	}
	if len(assembled.Evidence) != 0 {
		t.Fatalf("evidence = %+v, want none (retrieval failed)", assembled.Evidence)
	}
}

func TestIntegrationAssembleContextAllCandidatesBelowThresholdChargesNothingForRAG(t *testing.T) {
	db := testutil.MySQL(t, "conversation")
	repo := NewRepository(db)
	fakeKS := &fakeKnowledgeSvc{chunks: []knowledge.RetrievedChunk{
		{Chunk: knowledge.Chunk{ID: "c1", KnowledgeBaseID: "kb-1", DocumentID: "doc-1", Content: "无关内容"}, Score: 0.01},
	}}
	svc := newAssembleTestService(db, fakeKS)

	seedConversation(t, repo, "conv-lowscore", "ag-lowscore", "u1")
	seedHistory(t, repo, "conv-lowscore", 3, strings.Repeat("h", 100), "最新问题")

	ag := agent.Agent{ID: "ag-lowscore", ModelID: "m1", KnowledgeBaseIDs: []string{"kb-1"}}
	assembled, err := svc.assembleContext(context.Background(), "conv-lowscore", ag, smallWindowModel, "最新问题", "trace-1")
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	if len(assembled.Messages) != 4 {
		t.Fatalf("assembled %d messages, want all 4 history rows kept (every candidate filtered by score must not reserve RAG budget): %+v", len(assembled.Messages), assembled.Messages)
	}
	if assembled.FilteredByScore != 1 {
		t.Fatalf("FilteredByScore = %d, want 1", assembled.FilteredByScore)
	}
}

func TestIntegrationAssembleContextShortEvidenceReturnsUnusedBudgetToHistory(t *testing.T) {
	db := testutil.MySQL(t, "conversation")
	repo := NewRepository(db)
	fakeKS := &fakeKnowledgeSvc{chunks: []knowledge.RetrievedChunk{
		{Chunk: knowledge.Chunk{ID: "c1", KnowledgeBaseID: "kb-1", DocumentID: "doc-1", DocumentName: "a.md", Content: "短内容"}, Score: 0.9},
	}}
	svc := newAssembleTestService(db, fakeKS)

	seedConversation(t, repo, "conv-short-ev", "ag-short-ev", "u1")
	// 3 段历史 + 1 条最新，每段 100 字符——如果 RAG 仍然固定扣 2000 token
	// * 4 = 8000 字符（远超 2000 总预算，会被 clamp 到 2000，等于把
	// history 预算清零），这些历史会被裁掉；证据只有几个字，真正渲染后
	// 只占几十个字符，绝大部分"预算"应该还给 history。
	seedHistory(t, repo, "conv-short-ev", 3, strings.Repeat("h", 100), "最新问题")

	ag := agent.Agent{ID: "ag-short-ev", ModelID: "m1", KnowledgeBaseIDs: []string{"kb-1"}}
	assembled, err := svc.assembleContext(context.Background(), "conv-short-ev", ag, smallWindowModel, "最新问题", "trace-1")
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	if len(assembled.Evidence) != 1 {
		t.Fatalf("evidence = %+v, want exactly 1", assembled.Evidence)
	}
	// system prompt(0，未设置) + citation rules(system) + evidence(user) +
	// 4 条历史 = 6 条消息。只要历史全部保留就说明预算真的还给了 history，
	// 而不是被 2000-token 的固定 RAG 上限锁死。
	historyCount := 0
	for _, m := range assembled.Messages {
		if m.Content == strings.Repeat("h", 100) || m.Content == "最新问题" {
			historyCount++
		}
	}
	if historyCount != 4 {
		t.Fatalf("history messages present = %d, want 4 (short evidence must give unused RAG budget back to history), messages: %+v", historyCount, assembled.Messages)
	}
}

func TestIntegrationAssembleContextLatestUserMessageAlwaysKept(t *testing.T) {
	db := testutil.MySQL(t, "conversation")
	repo := NewRepository(db)
	fakeKS := &fakeKnowledgeSvc{chunks: []knowledge.RetrievedChunk{
		{Chunk: knowledge.Chunk{ID: "c1", KnowledgeBaseID: "kb-1", DocumentID: "doc-1", DocumentName: "a.md", Content: strings.Repeat("大", 2000)}, Score: 0.9},
	}}
	svc := newAssembleTestService(db, fakeKS)

	seedConversation(t, repo, "conv-keeplatest", "ag-keeplatest", "u1")
	// 大量历史 + 一个巨大的 evidence chunk，联手把预算挤到几乎没有剩余——
	// 即使这样，最新用户消息也必须出现在组装结果的最后一条。
	seedHistory(t, repo, "conv-keeplatest", 20, strings.Repeat("h", 200), "这是最新的真实问题")

	ag := agent.Agent{ID: "ag-keeplatest", ModelID: "m1", KnowledgeBaseIDs: []string{"kb-1"}}
	assembled, err := svc.assembleContext(context.Background(), "conv-keeplatest", ag, smallWindowModel, "这是最新的真实问题", "trace-1")
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	last := assembled.Messages[len(assembled.Messages)-1]
	if last.Role != provider.RoleUser || last.Content != "这是最新的真实问题" {
		t.Fatalf("last message = %+v, want the latest real user question", last)
	}
}

func TestIntegrationAssembleContextFinalEstimateNeverExceedsBudgetUnderSmallWindow(t *testing.T) {
	db := testutil.MySQL(t, "conversation")
	repo := NewRepository(db)
	fakeKS := &fakeKnowledgeSvc{chunks: []knowledge.RetrievedChunk{
		{Chunk: knowledge.Chunk{ID: "c1", KnowledgeBaseID: "kb-1", DocumentID: "doc-1",
			DocumentName: `很长的文档名 & <特殊字符> "引号"` + strings.Repeat("x", 100), Content: strings.Repeat("检索内容包含 <tag> & \"quote\" ", 100)}, Score: 0.9},
		{Chunk: knowledge.Chunk{ID: "c2", KnowledgeBaseID: "kb-1", DocumentID: "doc-2", DocumentName: "b.md", Content: strings.Repeat("另一段资料内容", 50)}, Score: 0.8},
	}}
	svc := newAssembleTestService(db, fakeKS)

	seedConversation(t, repo, "conv-estimate", "ag-estimate", "u1")
	seedHistory(t, repo, "conv-estimate", 30, strings.Repeat("历史消息内容", 20), "最终真实问题是什么")

	ag := agent.Agent{ID: "ag-estimate", ModelID: "m1", SystemPrompt: strings.Repeat("系统提示词", 20),
		KnowledgeBaseIDs: []string{"kb-1"}}
	// assembleContext itself doesn't take tools as a direct input (it
	// loads them via mcpSvc, which this test leaves nil, so len(tools)==0
	// both when assembleContext computed its own budget and here) — the
	// tool-cost half of the invariant is already covered by
	// TestComputeFixedBudgetChargesOnlySystemPromptAndTools; what this
	// test drives end-to-end is history+evidence+system-prompt staying
	// within budget together, via the real assembleContext pipeline.
	assembled, err := svc.assembleContext(context.Background(), "conv-estimate", ag, smallWindowModel, "最终真实问题是什么", "trace-1")
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}

	got := estimateRequestChars(assembled.Messages, assembled.Tools)
	want := totalBudgetChars(smallWindowModel)
	if got > want {
		t.Fatalf("estimateRequestChars = %d, exceeds totalBudgetChars %d under a small context window", got, want)
	}
}

var errFakeRetrieval = errors.New("向量库暂时不可用")

// --- 第二轮代码审查修复：最新消息重复计费 / byte-rune 计量单位统一 ---

func TestIntegrationAssembleContextOlderHistoryNotDroppedByLatestMessageDoubleCharge(t *testing.T) {
	// 精确构造一个"如果 latest message 被重复计费就会丢消息，不重复计费
	// 就不会丢"的场景：
	//   ContextWindow=3000, outputReserve=1000 -> totalBudgetChars=8000。
	//   latest 消息 100 个字符 -> computeFixedBudget 已经把这 100 个字符
	//   预留掉一次，fixedBudget=historyBudget=7900（没有 KB，没有证据）。
	//   两条 older 历史消息各 3950 个字符，合计正好 7900 —— 单独看完全
	//   放得下。
	// 如果 assembleContext 把包含 latest 的完整 rows 传给
	// truncateByBudget（旧 bug），从最新往最旧累加会是
	// latest(100)+older2(3950)+older1(3950)=8000，超过 7900 的预算，
	// older1 会被裁掉。修复后 latest 在传入 truncateByBudget 之前就已经
	// 分离出去，older 两条 7900 恰好=预算，两条都必须保留。
	db := testutil.MySQL(t, "conversation")
	repo := NewRepository(db)
	svc := &service{repo: repo, traceStore: trace.NewStore(db)}

	seedConversation(t, repo, "conv-doublecharge", "ag-doublecharge", "u1")
	ctx := context.Background()
	older1 := strings.Repeat("a", 3950)
	older2 := strings.Repeat("b", 3950)
	latest := strings.Repeat("c", 100)
	if err := repo.createMessage(ctx, Message{ID: platform.NewID(), ConversationID: "conv-doublecharge", Role: "user", Content: older1}); err != nil {
		t.Fatal(err)
	}
	if err := repo.createMessage(ctx, Message{ID: platform.NewID(), ConversationID: "conv-doublecharge", Role: "assistant", Content: older2}); err != nil {
		t.Fatal(err)
	}
	if err := repo.createMessage(ctx, Message{ID: platform.NewID(), ConversationID: "conv-doublecharge", Role: "user", Content: latest}); err != nil {
		t.Fatal(err)
	}

	model := provider.Model{ContextWindow: iptr(3000)} // totalBudgetChars = (3000-1000)*4 = 8000
	ag := agent.Agent{ID: "ag-doublecharge", ModelID: "m1"}
	assembled, err := svc.assembleContext(ctx, "conv-doublecharge", ag, model, latest, "trace-1")
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}

	if len(assembled.Messages) != 3 {
		t.Fatalf("assembled %d messages, want 3 (both older messages + latest, none dropped to make room for double-charged latest): %+v",
			len(assembled.Messages), summarizeContents(assembled.Messages))
	}
	if assembled.Messages[0].Content != older1 {
		t.Fatalf("oldest message missing/wrong — this is exactly what a double-charged latest message would evict first: got %.20s...", assembled.Messages[0].Content)
	}
	if assembled.Messages[1].Content != older2 {
		t.Fatalf("second message missing/wrong: got %.20s...", assembled.Messages[1].Content)
	}
	last := assembled.Messages[len(assembled.Messages)-1]
	if last.Role != provider.RoleUser || last.Content != latest {
		t.Fatalf("last message = %+v, want the latest user message", last)
	}
}

func TestIntegrationAssembleContextHistoryBudgetIsRuneBasedNotByteBased(t *testing.T) {
	// 中文历史消息：每条 500 个汉字（500 runes，1500 UTF-8 字节）。预算
	// 用 rune 计算时应该能放下一定数量；如果哪个环节偷偷换成字节数，
	// 同样的预算会因为"看起来占用了 3 倍空间"而把这些消息错误地裁掉。
	db := testutil.MySQL(t, "conversation")
	repo := NewRepository(db)
	svc := &service{repo: repo, traceStore: trace.NewStore(db)}

	seedConversation(t, repo, "conv-runebudget", "ag-runebudget", "u1")
	ctx := context.Background()
	chineseMsg := strings.Repeat("中", 500) // 500 runes, 1500 bytes
	latest := strings.Repeat("问", 10)      // 10 runes, 30 bytes
	if err := repo.createMessage(ctx, Message{ID: platform.NewID(), ConversationID: "conv-runebudget", Role: "user", Content: chineseMsg}); err != nil {
		t.Fatal(err)
	}
	if err := repo.createMessage(ctx, Message{ID: platform.NewID(), ConversationID: "conv-runebudget", Role: "user", Content: latest}); err != nil {
		t.Fatal(err)
	}

	// 精心选一个紧窗口，让"按 rune 算"和"按字节算"的结果分道扬镳：
	// ContextWindow=1152 -> budgetTokens=152 -> totalBudgetChars=608 ->
	// fixedBudget=608-10(latest)=598。500-rune 消息按 rune 算
	// (500<=598) 放得下；若被错误地按 UTF-8 字节数(1500)计量，598 根本
	// 放不下，会被裁掉——这正是本测试要抓的偏差。
	model := provider.Model{ContextWindow: iptr(1152)} // totalBudgetChars = (1152-1000)*4 = 608
	ag := agent.Agent{ID: "ag-runebudget", ModelID: "m1"}
	assembled, err := svc.assembleContext(ctx, "conv-runebudget", ag, model, latest, "trace-1")
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}

	found := false
	for _, m := range assembled.Messages {
		if m.Content == chineseMsg {
			found = true
		}
	}
	if !found {
		t.Fatalf("500-rune (1500-byte) Chinese history message was dropped — budget must be rune-based, not byte-based: messages = %+v", summarizeContents(assembled.Messages))
	}
}

func summarizeContents(messages []provider.Message) []string {
	out := make([]string, len(messages))
	for i, m := range messages {
		if len(m.Content) > 20 {
			out[i] = m.Content[:20] + "..."
		} else {
			out[i] = m.Content
		}
	}
	return out
}
