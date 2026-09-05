package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hify/internal/agent"
	"hify/internal/knowledge"
	"hify/internal/platform/trace"
	"hify/internal/testutil"
)

// 上下文组装确定性门禁（009-evidence-boundary-awareness）。
//
// ⭐ 它存在的理由与 Phase 6 的检索门禁完全平行，但守的是另一段链路：
// **上下文组装是整条对话链路上最容易被无声改坏的地方**。改错了不会报错、不会
// 有任何一条测试变红，只会让回答慢慢变差——而没有人会把"最近答得不太好"和三周前
// 某次改提示词的提交联系起来。
//
// 它证明的是「送给模型的消息序列正是我们打算送的」，**不证明**「回答变好了」。
// 后者本仓库测不了：make eval 带 LLM 裁判、同一份代码跑两次都不一致（宪法禁止
// 拿它当证据），而检索门禁只覆盖检索。这条边界必须记住，别把门禁绿了当成效果好。
//
// 六个场景里有两个是**回归基线**（有证据且文档完整、没绑知识库）：它们的价值不在
// 验证新功能，而在证明新功能**没有波及不该波及的轮次**。删掉它们门禁就只剩自证。
//
// Privacy: 报告里记录的是**送给模型的完整消息序列**，其中包含资料正文。这与
// 002 确立的"日志不记片段正文"口径不同——但门禁报告不是日志，它是本地
// eval/runs/ 下的开发者产物、由固定夹具生成、不含任何真实用户数据。

const contextGateReportPathEnv = "HIFY_CONTEXT_GATE_REPORT_PATH"

func contextGateReportPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv(contextGateReportPathEnv); p != "" {
		return p
	}
	return filepath.Join(t.TempDir(), "phase16-context-gate-latest.json")
}

type contextGateMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type contextGateCase struct {
	Name          string               `json:"name"`
	Messages      []contextGateMessage `json:"messages"`
	EvidenceCount int                  `json:"evidence_count"`
}

type contextGateReport struct {
	RanAt time.Time         `json:"ran_at"`
	Cases []contextGateCase `json:"cases"`
}

var errGateRetrieval = errors.New("gate: retrieval unavailable")

// gateChunk builds one retrieved chunk with a stable, readable body.
func gateChunk(docID, docName, content string) knowledge.RetrievedChunk {
	return knowledge.RetrievedChunk{
		Chunk: knowledge.Chunk{
			ID: "chunk-" + docID, KnowledgeBaseID: "kb-gate", DocumentID: docID,
			DocumentName: docName, ChunkIndex: 0, Content: content,
		},
		Score: 0.9,
	}
}

// TestContextGate runs every scenario through assembleContext and records
// the exact message sequence handed to the model.
//
// ⚠️ When a case's recorded output changes, that is a BEHAVIOUR CHANGE to
// what the model sees — treat it exactly like a code change, not like a
// test that needs updating. The two regression cases in particular
// (evidence_present_complete_docs, no_knowledge_bases) must not move
// unless the change was deliberately about them.
func TestContextGate(t *testing.T) {
	db := testutil.MySQL(t, "conversation")
	repo := NewRepository(db)

	report := contextGateReport{RanAt: time.Now().UTC()}

	record := func(name string, svc *service, ag agent.Agent, convID string) {
		t.Helper()
		seedConversation(t, repo, convID, ag.ID, "u1")
		seedHistory(t, repo, convID, 2, "历史消息内容", "用户的最新问题")

		assembled, err := svc.assembleContext(context.Background(), convID, ag, smallWindowModel, "用户的最新问题", "trace-gate")
		if err != nil {
			t.Fatalf("case %q: assembleContext: %v", name, err)
		}
		msgs := make([]contextGateMessage, 0, len(assembled.Messages))
		for _, m := range assembled.Messages {
			msgs = append(msgs, contextGateMessage{Role: string(m.Role), Content: m.Content})
		}
		report.Cases = append(report.Cases, contextGateCase{
			Name: name, Messages: msgs, EvidenceCount: len(assembled.Evidence),
		})
	}

	agWithKB := agent.Agent{ID: "ag-gate", ModelID: "m1", KnowledgeBaseIDs: []string{"kb-gate"}}
	agNoKB := agent.Agent{ID: "ag-gate-nokb", ModelID: "m1"}

	// 1. 回归基线：有证据、命中文档全部完整。⚠️ 这一条改动前后必须逐字节一致。
	record("evidence_present_complete_docs",
		newAssembleTestService(db, &fakeKnowledgeSvc{chunks: []knowledge.RetrievedChunk{
			gateChunk("doc-a", "手册A.pdf", "这是第一条命中的资料正文。"),
			gateChunk("doc-b", "手册B.pdf", "这是第二条命中的资料正文。"),
		}}),
		agWithKB, "conv-gate-1")

	// 2. 有证据、部分命中文档不完整（009 US2 新增行为）。
	//
	// ⚠️ incompleteDocs 必须真的配上——第一版这个场景只是把文档**起名叫**
	// doc-incomplete 就以为在测不完整文档了，而 fake 返回 nil，于是它测的其实是
	// 「完整文档」，和场景 1 完全重复。门禁把这件事抓了出来（改动前后逐字节没变，
	// 而它本该变），否则这个场景会以一个自欺的形态长期绿着。
	record("evidence_present_incomplete_docs",
		newAssembleTestService(db, &fakeKnowledgeSvc{
			chunks: []knowledge.RetrievedChunk{
				gateChunk("doc-incomplete", "合同.pdf", "这是来自一份不完整文档的资料。"),
				gateChunk("doc-a", "手册A.pdf", "这是来自一份完整文档的资料。"),
			},
			incompleteDocs: map[string]knowledge.DocumentCoverage{
				"doc-incomplete": {
					DocumentID:       "doc-incomplete",
					UnextractedPages: []int{46, 47, 48, 49, 50},
					UnparseablePages: []int{1},
				},
			},
		}),
		agWithKB, "conv-gate-2")

	// 3. 检索执行、无匹配（009 US1 新增行为）。
	record("retrieval_executed_no_match",
		newAssembleTestService(db, &fakeKnowledgeSvc{chunks: nil}),
		agWithKB, "conv-gate-3")

	// 4. 回归基线：没绑知识库。⚠️ 改动前后必须逐字节一致，且绝不能出现"已检索"
	//    一类的话——没查过就不能说查过。
	record("no_knowledge_bases",
		&service{repo: repo, traceStore: trace.NewStore(db)},
		agNoKB, "conv-gate-4")

	// 5. 检索失败降级为空：不得注入任何信号。把基础设施故障说成"知识库里没有"，
	//    是拿故障冒充事实结论。
	record("retrieval_failed",
		newAssembleTestService(db, &fakeKnowledgeSvc{err: errGateRetrieval}),
		agWithKB, "conv-gate-5")

	// 6. 多轮：上一轮有证据、本轮空检索——信号必须逐轮判断，不得残留。
	record("multi_turn_evidence_then_empty",
		newAssembleTestService(db, &fakeKnowledgeSvc{chunks: nil}),
		agWithKB, "conv-gate-6")

	path := contextGateReportPath(t)
	blob, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir report dir: %v", err)
	}
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("context gate report written to %s (%d cases)", path, len(report.Cases))
}

// TestContextGateIsDeterministic — 同一输入连续跑，产出必须完全相同（SC-008）。
// 组装过程里任何依赖 map 迭代顺序的地方都会在这里暴露。
func TestContextGateIsDeterministic(t *testing.T) {
	db := testutil.MySQL(t, "conversation")
	repo := NewRepository(db)
	svc := newAssembleTestService(db, &fakeKnowledgeSvc{chunks: []knowledge.RetrievedChunk{
		gateChunk("doc-a", "手册A.pdf", "资料一"),
		gateChunk("doc-b", "手册B.pdf", "资料二"),
		gateChunk("doc-c", "手册C.pdf", "资料三"),
	}})
	ag := agent.Agent{ID: "ag-det", ModelID: "m1", KnowledgeBaseIDs: []string{"kb-gate"}}
	seedConversation(t, repo, "conv-det", ag.ID, "u1")
	seedHistory(t, repo, "conv-det", 2, "历史", "问题")

	render := func() string {
		assembled, err := svc.assembleContext(context.Background(), "conv-det", ag, smallWindowModel, "问题", "trace-det")
		if err != nil {
			t.Fatalf("assembleContext: %v", err)
		}
		var sb strings.Builder
		for _, m := range assembled.Messages {
			sb.WriteString(string(m.Role))
			sb.WriteString("|")
			sb.WriteString(m.Content)
			sb.WriteString("\n---\n")
		}
		return sb.String()
	}
	want := render()
	for i := 0; i < 30; i++ {
		if got := render(); got != want {
			t.Fatalf("第 %d 次组装结果与第 0 次不同——组装过程依赖了某种不确定的顺序", i)
		}
	}
}
