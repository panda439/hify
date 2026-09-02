package agent

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"hify/internal/knowledge"
)

// 004-agent-document-scope：文档范围两条校验规则的纯逻辑单测。
// 不连数据库——validateScopedDocuments 只依赖 knowledge.Service 的 GetDocument，
// 那是天然的缝隙。

// fakeKnowledgeSvcForScope 只实现 GetDocument，其余方法靠嵌入接口占位
// （本文件不会调到它们，调到就是 nil panic，那正好说明测试写错了）。
type fakeKnowledgeSvcForScope struct {
	knowledge.Service
	// docs: documentID -> 它所属的 knowledgeBaseID
	docs map[string]string
}

func (f *fakeKnowledgeSvcForScope) GetDocument(_ context.Context, id string) (knowledge.Document, error) {
	kbID, ok := f.docs[id]
	if !ok {
		return knowledge.Document{}, knowledge.ErrDocumentNotFound
	}
	return knowledge.Document{ID: id, KnowledgeBaseID: kbID}, nil
}

func newScopeService(docs map[string]string) *service {
	return &service{knowledgeSvc: &fakeKnowledgeSvcForScope{docs: docs}}
}

func TestValidateScopedDocumentsEmptyScopeIsAlwaysValid(t *testing.T) {
	s := newScopeService(nil)
	for _, tc := range []struct {
		name string
		docs []string
	}{
		{"nil", nil},
		{"空切片", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 空范围 = 不限定，必须永远合法：把"我没有限定任何东西"变成一个
			// 错误，会让所有现存的、没配范围的 Agent 都保存不了。
			if err := s.validateScopedDocuments(context.Background(), []string{"kb-1"}, tc.docs); err != nil {
				t.Fatalf("空范围必须合法，got %v", err)
			}
		})
	}
}

func TestValidateScopedDocumentsAcceptsDocumentsInBoundKnowledgeBases(t *testing.T) {
	s := newScopeService(map[string]string{"doc-a": "kb-1", "doc-b": "kb-2"})
	if err := s.validateScopedDocuments(context.Background(), []string{"kb-1", "kb-2"}, []string{"doc-a", "doc-b"}); err != nil {
		t.Fatalf("两份文档都属于已绑定的知识库，应当合法，got %v", err)
	}
}

func TestValidateScopedDocumentsRejectsDocumentOutsideBoundKnowledgeBases(t *testing.T) {
	s := newScopeService(map[string]string{"doc-a": "kb-1", "doc-other": "kb-99"})

	// doc-other 属于 kb-99，而 Agent 只绑了 kb-1。
	// 这种配置不会让检索报错（002 的 FR-010：未知文档 = 无匹配），正因如此
	// 才必须在保存时拒绝——否则用户会保存成功，然后发现 Agent 什么都不知道。
	err := s.validateScopedDocuments(context.Background(), []string{"kb-1"}, []string{"doc-a", "doc-other"})
	if !errors.Is(err, ErrInvalidScopedDocument) {
		t.Fatalf("want ErrInvalidScopedDocument, got %v", err)
	}
}

func TestValidateScopedDocumentsRejectsUnknownDocument(t *testing.T) {
	s := newScopeService(map[string]string{"doc-a": "kb-1"})
	err := s.validateScopedDocuments(context.Background(), []string{"kb-1"}, []string{"doc-ghost"})
	if !errors.Is(err, ErrInvalidScopedDocument) {
		t.Fatalf("不存在的文档应当归入同一类配置错误，got %v", err)
	}
}

// TestValidateScopedDocumentsRejectsRatherThanTruncates —— FR-005。
// 上限必须是拒绝，不是截断：静默截断会把用户配的范围悄悄改小，
// 与 002 的 FR-009「不得自动放宽/改变已指定的过滤条件」是同一条原则。
func TestValidateScopedDocumentsRejectsRatherThanTruncates(t *testing.T) {
	docs := map[string]string{}
	ids := make([]string, knowledge.MaxFilterDocumentIDs+1)
	for i := range ids {
		id := "doc-" + strconv.Itoa(i)
		ids[i] = id
		docs[id] = "kb-1"
	}
	s := newScopeService(docs)

	err := s.validateScopedDocuments(context.Background(), []string{"kb-1"}, ids)
	if !errors.Is(err, ErrTooManyScopedDocuments) {
		t.Fatalf("want ErrTooManyScopedDocuments, got %v", err)
	}
	if len(ids) != knowledge.MaxFilterDocumentIDs+1 {
		t.Fatalf("校验不得改写调用方的切片：len = %d", len(ids))
	}
}

func TestValidateScopedDocumentsAcceptsExactlyTheLimit(t *testing.T) {
	docs := map[string]string{}
	ids := make([]string, knowledge.MaxFilterDocumentIDs)
	for i := range ids {
		id := "doc-" + strconv.Itoa(i)
		ids[i] = id
		docs[id] = "kb-1"
	}
	s := newScopeService(docs)
	if err := s.validateScopedDocuments(context.Background(), []string{"kb-1"}, ids); err != nil {
		t.Fatalf("恰好等于上限应当合法，got %v", err)
	}
}

// TestScopeLimitMatchesKnowledgeFilterLimit 锁定"两个上限必须相等"。
// agent 侧保存时的上限如果比 knowledge 侧过滤器的上限宽，用户能存下一个
// 检索时必然被拒的配置；反过来则是无谓地更严。它们表达的是同一条规则，
// 所以 agent 直接用 knowledge 导出的常量，这条断言防止将来有人在这里
// 复制出一个字面量 50。
func TestScopeLimitMatchesKnowledgeFilterLimit(t *testing.T) {
	if knowledge.MaxFilterDocumentIDs != 50 {
		t.Fatalf("上限变了（现在是 %d）——请同步确认 agent 侧校验、前端提示文案和"+
			"002/004 两份规格里写的数字", knowledge.MaxFilterDocumentIDs)
	}
}
