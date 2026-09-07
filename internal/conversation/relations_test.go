package conversation

import (
	"context"
	"strings"
	"testing"
	"time"

	"hify/internal/agent"
	"hify/internal/knowledge"
	"hify/internal/platform/trace"
	"hify/internal/provider"
	"hify/internal/testutil"
)

// relations_test.go 守聊天里的关系分支（010 T033/T034）。
//
// ⭐ 用例的重心：**没有记录时绝不让模型开口**。一个没有原文支持、却读起来
// 完全正常的回答，比答不上来糟糕得多——用户分辨不出这一条和有引用的那一条
// 是两种东西。所以除 found 之外的每一种结局，模型的调用次数都必须是 0。

// fakeRelationKnowledge 是可编程的关系查询假实现。
type fakeRelationKnowledge struct {
	knowledge.Service
	res     knowledge.RelationQueryResult
	err     error
	queries []knowledge.RelationQuery
}

// ⚠️ 普通对话路径也会走到 Retrieve：嵌入 knowledge.Service 只是省掉其余
// 方法，被真的调到时那是个 nil 接口，会 panic。
func (f *fakeRelationKnowledge) Retrieve(context.Context, []string, string, int, knowledge.RetrieveOptions) ([]knowledge.RetrievedChunk, error) {
	return nil, nil
}

func (f *fakeRelationKnowledge) DocumentCoverages(context.Context, []string) (map[string]knowledge.DocumentCoverage, error) {
	return nil, nil
}

func (f *fakeRelationKnowledge) QueryRelations(_ context.Context, q knowledge.RelationQuery) (knowledge.RelationQueryResult, error) {
	f.queries = append(f.queries, q)
	return f.res, f.err
}

func relationRecords() []knowledge.RelationRecord {
	ch2 := 2
	ch7 := 7
	return []knowledge.RelationRecord{
		{RelationID: "r1", Type: "欺凌", IsDirected: true,
			SubjectName: "赵太爷", ObjectName: "阿Q", ChapterNumber: &ch2, SourceOrder: 10,
			Evidence: []knowledge.RelationEvidenceItem{{
				ChunkID: "c-1", DocumentID: "doc-1", DocumentName: "阿Q正传.txt",
				Quote: "赵太爷跳过去给了他一个嘴巴", SourceOrder: 10,
			}}},
		{RelationID: "r2", Type: "雇佣", IsDirected: true,
			SubjectName: "赵太爷", ObjectName: "阿Q", ChapterNumber: &ch7, SourceOrder: 90,
			Evidence: []knowledge.RelationEvidenceItem{{
				ChunkID: "c-9", DocumentID: "doc-1", DocumentName: "阿Q正传.txt",
				Quote: "叫阿Q来舂米", SourceOrder: 90,
			}}},
	}
}

func totalItems(n int) *int { return &n }

func relationTestService(t *testing.T, chat *scriptedChatClient, kn *fakeRelationKnowledge, convID string) (Service, *Repository) {
	t.Helper()
	db := testutil.MySQL(t, "conversation")
	repo := NewRepository(db)
	svc := NewService(repo,
		&fakeAgentSvc{ag: agent.Agent{ID: "ag-rel", ModelID: "m1", SystemPrompt: "你是助手",
			KnowledgeBaseIDs: []string{"kb-rel"}, DocumentIDs: []string{"doc-1"},
			Temperature: 0.7, MaxTokens: intp(1000)}},
		&fakeProviderSvc{client: chat}, kn, &fakeMCPSvc{}, trace.NewStore(db),
		// 改写开着也不该影响关系分支——它在这条路上根本不该被调用。
		true, "", 1500*time.Millisecond,
	)
	seedConversation(t, repo, convID, "ag-rel", "u1")
	return svc, repo
}

func relationOpts() MessageOptions {
	return MessageOptions{Relation: &RelationQueryOption{
		DocumentID: "doc-1", Subject: "赵太爷", Object: "阿Q",
	}}
}

// TestRelationBranchAnswersFromEvidenceOnly：有记录时进入受限生成，
// 每一章的记录都要进提示词，且**不带工具**。
func TestRelationBranchAnswersFromEvidenceOnly(t *testing.T) {
	chat := &scriptedChatClient{scripts: [][]provider.ChatChunk{{
		{DeltaContent: "第二章里赵太爷打了阿Q[S1]，第七章雇他舂米[S2]。"},
		{FinishReason: "stop"},
	}}}
	kn := &fakeRelationKnowledge{res: knowledge.RelationQueryResult{
		Status: knowledge.RelationStatusFound, Records: relationRecords(),
		Coverage: knowledge.RelationCoverage{State: "succeeded", TotalItems: totalItems(9),
			SucceededItems: 9, Complete: true},
	}}
	svc, _ := relationTestService(t, chat, kn, "conv-rel-1")

	events, err := svc.StreamMessageWithOptions(context.Background(), "u1", "conv-rel-1",
		"赵太爷和阿Q是什么关系", relationOpts())
	if err != nil {
		t.Fatalf("StreamMessageWithOptions: %v", err)
	}
	got := drainEvents(t, events)
	if last := eventTypes(got); last[len(last)-1] != EventDone {
		t.Fatalf("事件序列 = %v", last)
	}

	// 范围来自 Agent 的配置，不是请求体。
	if len(kn.queries) != 1 {
		t.Fatalf("QueryRelations 调用了 %d 次", len(kn.queries))
	}
	q := kn.queries[0]
	if len(q.Scope.KnowledgeBaseIDs) != 1 || q.Scope.KnowledgeBaseIDs[0] != "kb-rel" {
		t.Errorf("KB 范围没有来自 Agent：%+v", q.Scope)
	}
	if len(q.Scope.DocumentIDs) != 1 || q.Scope.DocumentIDs[0] != "doc-1" {
		t.Errorf("文档范围没有下推：%+v", q.Scope)
	}

	if len(chat.requests) != 1 {
		t.Fatalf("模型调用了 %d 次", len(chat.requests))
	}
	req := chat.requests[0]
	// ⚠️ 关系分支关掉工具循环。
	if len(req.Tools) != 0 {
		t.Errorf("关系分支带上了工具：%+v", req.Tools)
	}
	var prompt strings.Builder
	for _, m := range req.Messages {
		prompt.WriteString(string(m.Role))
		prompt.WriteString(": ")
		prompt.WriteString(m.Content)
		prompt.WriteString("\n")
	}
	text := prompt.String()
	for _, want := range []string{"赵太爷跳过去给了他一个嘴巴", "叫阿Q来舂米", "第 2 章", "第 7 章"} {
		if !strings.Contains(text, want) {
			t.Errorf("提示词里缺少 %q", want)
		}
	}
	// 覆盖提示是服务端稳定附加文本，不能只靠模型记得。
	if !strings.Contains(text, "已抽取全部 9 个可读片段") {
		t.Errorf("提示词里没有覆盖提示：\n%s", text)
	}
	// 系统提示词仍然排在最前面，助手人格不被这条分支顶掉。
	if req.Messages[0].Role != provider.RoleSystem || req.Messages[0].Content != "你是助手" {
		t.Errorf("Agent 的 system prompt 不在第一条：%+v", req.Messages[0])
	}

	final := got[len(got)-2]
	if len(final.Citations) != 2 {
		t.Fatalf("引用数 = %d，回答里引了 [S1][S2]", len(final.Citations))
	}
	for _, c := range final.Citations {
		if c.Score != 0 {
			t.Errorf("关系证据不是向量召回，Score 必须是 0，得到 %v", c.Score)
		}
	}
}

// TestRelationBranchNeverCallsTheModelWithoutRecords：四种"没有答案"的结局
// 一律确定性回复，模型调用次数必须是 0。
func TestRelationBranchNeverCallsTheModelWithoutRecords(t *testing.T) {
	cases := []struct {
		name string
		res  knowledge.RelationQueryResult
		want string
	}{
		{"没有记录", knowledge.RelationQueryResult{
			Status:   knowledge.RelationStatusNotFound,
			Coverage: knowledge.RelationCoverage{TotalItems: totalItems(9), SucceededItems: 4},
		}, "没有找到这两个人之间的关系"},
		{"未开启", knowledge.RelationQueryResult{Status: knowledge.RelationStatusDisabled}, "还没有开启人物关系抽取"},
		{"语料已更新", knowledge.RelationQueryResult{Status: knowledge.RelationStatusIncomplete}, "重新抽取"},
		{"同一个人", knowledge.RelationQueryResult{Status: knowledge.RelationStatusSameEntity}, "指向同一个人物"},
		{"需要澄清", knowledge.RelationQueryResult{
			Status: knowledge.RelationStatusAmbiguous,
			ObjectCandidates: []knowledge.RelationCharacterCandidate{
				{CharacterID: "c1", DisplayName: "阿Q", Context: "首次出现于第 1 个片段"},
				{CharacterID: "c2", DisplayName: "阿Q", Context: "首次出现于第 40 个片段"},
			},
		}, "对应不止一个人物"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chat := &scriptedChatClient{}
			kn := &fakeRelationKnowledge{res: tc.res}
			convID := "conv-rel-empty-" + string(rune('a'+i))
			svc, repo := relationTestService(t, chat, kn, convID)

			events, err := svc.StreamMessageWithOptions(context.Background(), "u1", convID,
				"赵太爷和阿Q是什么关系", relationOpts())
			if err != nil {
				t.Fatalf("StreamMessageWithOptions: %v", err)
			}
			got := drainEvents(t, events)
			if len(chat.requests) != 0 {
				t.Fatalf("没有记录却调用了模型 %d 次", len(chat.requests))
			}
			final := got[len(got)-2]
			if !strings.Contains(final.Content, tc.want) {
				t.Errorf("回复里没有 %q：%s", tc.want, final.Content)
			}
			// ⭐ 确定性回复也要落库：不落的话刷新之后这一轮就消失了，
			// 用户会以为自己没问过。
			msgs, err := repo.listRecentMessages(context.Background(), convID, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 2 {
				t.Fatalf("会话里有 %d 条消息，应当是提问 + 回复", len(msgs))
			}
			// ⚠️ 内部标识不能出现在用户看到的文本里。
			for _, bad := range []string{"character_id", "job_id", "epoch", "c1", "c2"} {
				if strings.Contains(final.Content, bad) {
					t.Errorf("回复里出现了内部标识 %q：%s", bad, final.Content)
				}
			}
		})
	}
}

// TestRelationQueryFailureIsAnError：读取故障在 SSE 之前按 HTTP 错误上抛，
// ⚠️ 绝不伪装成一条"没有找到关系"的助手回复。
func TestRelationQueryFailureIsAnError(t *testing.T) {
	chat := &scriptedChatClient{}
	kn := &fakeRelationKnowledge{err: knowledge.ErrRelationQueryUnavailable}
	svc, repo := relationTestService(t, chat, kn, "conv-rel-err")

	_, err := svc.StreamMessageWithOptions(context.Background(), "u1", "conv-rel-err",
		"赵太爷和阿Q是什么关系", relationOpts())
	if err == nil {
		t.Fatal("读取故障被当成了正常结果")
	}
	if len(chat.requests) != 0 {
		t.Errorf("故障之后还调用了模型 %d 次", len(chat.requests))
	}
	// 用户的提问仍然留在会话里——分叉发生在它落库之后。
	msgs, err := repo.listRecentMessages(context.Background(), "conv-rel-err", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("会话里有 %d 条消息，应当只留下用户的提问", len(msgs))
	}
}

// TestPlainChatIsUnchanged：不带 relation_query 时行为与这个功能上线前一致。
func TestPlainChatIsUnchanged(t *testing.T) {
	chat := &scriptedChatClient{scripts: [][]provider.ChatChunk{{
		{DeltaContent: "普通回答。"}, {FinishReason: "stop"},
	}}}
	kn := &fakeRelationKnowledge{}
	svc, _ := relationTestService(t, chat, kn, "conv-rel-plain")

	events, err := svc.StreamMessage(context.Background(), "u1", "conv-rel-plain", "你好")
	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}
	drainEvents(t, events)
	if len(kn.queries) != 0 {
		t.Errorf("普通对话调用了关系查询 %d 次", len(kn.queries))
	}
	if len(chat.requests) != 1 {
		t.Fatalf("模型调用了 %d 次", len(chat.requests))
	}
}

// TestRelationEvidenceDropsWholeItemsWhenBudgetIsTight：预算不够时整条丢，
// ⚠️ 绝不把引用裁成半句——半句引用仍然会被当成"原文这么写的"。
func TestRelationEvidenceDropsWholeItemsWhenBudgetIsTight(t *testing.T) {
	records := relationRecords()
	full, dropped := relationEvidence(records, 10000)
	if len(full) != 2 || dropped != 0 {
		t.Fatalf("预算充足时应当两条都在：%d 条，丢弃 %d", len(full), dropped)
	}
	// 只够放第一条。
	one := len([]rune(formatSource(full[0])))
	got, dropped := relationEvidence(records, one)
	if len(got) != 1 || dropped != 1 {
		t.Fatalf("紧预算下 = %d 条，丢弃 %d，应当是 1/1", len(got), dropped)
	}
	if !strings.Contains(got[0].Content, "赵太爷跳过去给了他一个嘴巴") {
		t.Errorf("留下来的那条被裁短了：%q", got[0].Content)
	}
	// 一条都放不下。
	got, _ = relationEvidence(records, 1)
	if len(got) != 0 {
		t.Fatalf("放不下时应当一条都不给，得到 %d 条", len(got))
	}
}

// TestNoBudgetGivesADeterministicReply：一条证据都放不下时给固定回复，
// 不生成没有依据的结论。
func TestNoBudgetGivesADeterministicReply(t *testing.T) {
	chat := &scriptedChatClient{}
	kn := &fakeRelationKnowledge{res: knowledge.RelationQueryResult{
		Status: knowledge.RelationStatusFound,
		Records: []knowledge.RelationRecord{{
			RelationID: "r1", Type: "欺凌", SubjectName: "赵太爷", ObjectName: "阿Q",
			Evidence: []knowledge.RelationEvidenceItem{{
				ChunkID: "c-1", DocumentID: "doc-1", DocumentName: "阿Q正传.txt",
				Quote: strings.Repeat("很长的原文", 5000),
			}},
		}},
		Coverage: knowledge.RelationCoverage{TotalItems: totalItems(1), SucceededItems: 1, Complete: true},
	}}
	svc, _ := relationTestService(t, chat, kn, "conv-rel-budget")

	events, err := svc.StreamMessageWithOptions(context.Background(), "u1", "conv-rel-budget",
		"赵太爷和阿Q是什么关系", relationOpts())
	if err != nil {
		t.Fatalf("StreamMessageWithOptions: %v", err)
	}
	got := drainEvents(t, events)
	if len(chat.requests) != 0 {
		t.Fatalf("放不下依据却还是调用了模型 %d 次", len(chat.requests))
	}
	if final := got[len(got)-2]; !strings.Contains(final.Content, "上下文预算不足") {
		t.Errorf("回复 = %q", final.Content)
	}
}

// TestRelationOptionValidation：连人名都没给的请求在最前面就被挡掉。
func TestRelationOptionValidation(t *testing.T) {
	chat := &scriptedChatClient{}
	kn := &fakeRelationKnowledge{}
	svc, _ := relationTestService(t, chat, kn, "conv-rel-invalid")

	for _, opt := range []RelationQueryOption{
		{DocumentID: "", Subject: "甲", Object: "乙"},
		{DocumentID: "doc-1", Subject: " ", Object: "乙"},
		{DocumentID: "doc-1", Subject: "甲", Object: ""},
	} {
		_, err := svc.StreamMessageWithOptions(context.Background(), "u1", "conv-rel-invalid",
			"问题", MessageOptions{Relation: &opt})
		if err == nil {
			t.Errorf("不完整的关系查询被接受了：%+v", opt)
		}
	}
	if len(kn.queries) != 0 {
		t.Errorf("非法请求走到了查询层 %d 次", len(kn.queries))
	}
}
