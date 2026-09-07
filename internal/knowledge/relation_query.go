package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"hify/internal/db/gen"
)

// relation_query.go 回答「A 和 B 是什么关系」（010 T031）。
//
// ⭐ 这是这一期立项的那个问题真正被回答的地方。
//
// ⚠️ 这一层最要紧的不是"能查到"，而是**把几种"查不到"分清楚**：
//   - 范围外：这份文档不在当前 Agent 能看的范围里；
//   - 没开启抽取：这份文档根本没做过关系抽取；
//   - 抽取未完成：开了但还没跑完，已跑的部分里没有；
//   - 名字不认识：书里没有这个称呼；
//   - 同一个人：两个名字指向同一个实体；
//   - 确实没有：跑完了，书里就是没写这两个人的关系。
//
// 六种情况给用户的下一步完全不同，而把它们都答成"没有找到相关信息"
// 是最省事也最没用的做法——用户无法判断该等一等、该重跑、还是该换个问法。

type relationQueryOutcome string

const (
	relationQueryFound        relationQueryOutcome = "found"
	relationQueryNoRecords    relationQueryOutcome = "no_records"
	relationQueryIncomplete   relationQueryOutcome = "incomplete"
	relationQueryNotExtracted relationQueryOutcome = "not_extracted"
	relationQueryUnknownName  relationQueryOutcome = "unknown_name"
	relationQueryAmbiguous    relationQueryOutcome = "ambiguous"
	relationQuerySameEntity   relationQueryOutcome = "same_entity"
	relationQueryOutOfScope   relationQueryOutcome = "out_of_scope"
	// relationQueryStale：查询期间文档被删除/改版，或者一次新的抽取接管了
	// 这份文档。⚠️ 它**不是**"查不到"的一种——手上这批记录属于一个已经
	// 不存在的上下文，答出去就是拿上一轮的结论配这一轮的进度。
	relationQueryStale relationQueryOutcome = "stale"
)

const (
	// maxRelationCandidates 是同名候选的展示上限。
	maxRelationCandidates = 8
	// maxRelationRecords 是一次查询返回的关系上限。
	// ⚠️ 取 LIMIT+1 判断是否截断——不判的话，一个关系很多的人物对
	// 会安静地少返回几条，而回答看起来是完整的。
	maxRelationRecords = 200
)

type relationQueryInput struct {
	// DocumentIDs 是**已经下推过 Agent 范围**的文档列表。
	// ⚠️ 空列表表示"没有任何可查的文档"，**不是**"不限定"。
	// 这是 002/004 已经确立的口径：空范围绝不静默放宽成全库，
	// 否则 Agent 会悄悄用起范围外的资料，而回答看起来完全正常。
	DocumentIDs []string
	Subject     string
	Object      string
	SubjectID   string
	ObjectID    string
}

type relationCandidate struct {
	ID               string
	DisplayName      string
	FirstSourceOrder int64
	HasAmbiguity     bool
	QueryRole        string
}

type relationEvidenceRecord struct {
	Quote       string
	SourceStart int
	SourceEnd   int
	SourceOrder int64
	ChunkID     string
}

type relationRecord struct {
	Type             string
	IsDirected       bool
	SubjectName      string
	ObjectName       string
	FirstSourceOrder int64
	ChapterNumber    *int
	ChapterTitle     *string
	Evidence         []relationEvidenceRecord
}

type relationQueryResult struct {
	Outcome    relationQueryOutcome
	DocumentID string
	// DocumentVersion / JobID 是**核验的坐标系**（010 T034）：
	// 一条证据的 source_start/source_end 是文档 rune 坐标，只有配上
	// "哪个版本"才有意义；JobID 则用来判断这一轮读到的记录是否已经被
	// 一次新的抽取取代。
	DocumentVersion int64
	JobID           string
	Relations       []relationRecord
	Candidates      []relationCandidate
	// UnknownName 是没能在书里找到的那个称呼。
	// ⚠️ 不指出是哪个名字的话，用户把名字打错了却以为书里真的没写他们的关系。
	UnknownName string
	// RemainingItems 只在 incomplete 时有值：用户要据此判断该不该等。
	RemainingItems *int
	Truncated      bool
}

// queryRelations 在给定范围内回答一次关系查询。
func (r *Repository) queryRelations(ctx context.Context, in relationQueryInput) (relationQueryResult, error) {
	if len(in.DocumentIDs) == 0 {
		// ⭐ 空范围不是全库。见 relationQueryInput.DocumentIDs 的注释。
		return relationQueryResult{Outcome: relationQueryOutOfScope}, nil
	}

	// ⚠️ 逐个文档试：一次查询可能覆盖多份文档，而每份文档的抽取状态不同。
	// 先找到第一份"能回答"的，把它的结论返回；都不能回答时返回最能说明
	// 情况的那一个（未开启 < 未完成 < 无记录）。
	best := relationQueryResult{Outcome: relationQueryNotExtracted}
	rank := map[relationQueryOutcome]int{
		relationQueryNotExtracted: 0, relationQueryIncomplete: 1,
		relationQueryUnknownName: 2, relationQueryNoRecords: 3,
		relationQuerySameEntity: 4, relationQueryAmbiguous: 5, relationQueryFound: 6,
	}
	for _, docID := range in.DocumentIDs {
		res, err := r.queryRelationsInDocument(ctx, docID, in)
		if err != nil {
			return relationQueryResult{}, err
		}
		if rank[res.Outcome] > rank[best.Outcome] {
			best = res
		}
		if res.Outcome == relationQueryFound {
			return res, nil
		}
	}
	return best, nil
}

func (r *Repository) queryRelationsInDocument(ctx context.Context, docID string, in relationQueryInput) (relationQueryResult, error) {
	out := relationQueryResult{DocumentID: docID, Outcome: relationQueryNotExtracted}

	doc, err := r.queries.GetDocumentExtractionState(ctx, docID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// ⚠️ 文档不存在与"不在范围内"对用户是同一件事，
			// 但都**不是** no_records：那会让用户以为书里真的没写。
			return relationQueryResult{Outcome: relationQueryOutOfScope}, nil
		}
		return out, fmt.Errorf("knowledge: load document for relation query: %w", err)
	}
	if !doc.IsRelationExtractionEnabled || !doc.ActiveRelationJobID.Valid {
		return out, nil
	}
	jobID := doc.ActiveRelationJobID.String
	out.JobID, out.DocumentVersion = jobID, doc.Version

	// ⭐ 只读文档**当前指向**的那个 run。⚠️ 读到旧 run 的记录，用户会看到
	// 自己已经"重新开始"过的那一次的结果，而界面显示的是新 run 的进度。
	job, err := r.queries.GetRelationExtractionJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return out, fmt.Errorf("knowledge: load job for relation query: %w", err)
	}

	subjects, err := r.findCharacters(ctx, jobID, in.Subject, in.SubjectID)
	if err != nil {
		return out, err
	}
	objects, err := r.findCharacters(ctx, jobID, in.Object, in.ObjectID)
	if err != nil {
		return out, err
	}

	incomplete := !job.InitializationComplete ||
		job.SucceededItems+job.FailedItems < job.TotalItems
	remaining := int(job.TotalItems - job.SucceededItems - job.FailedItems)

	if len(subjects) == 0 || len(objects) == 0 {
		// ⚠️ 抽取还没跑完时，"没找到这个名字"更可能是还没抽到，
		// 而不是书里没有——所以未完成优先于 unknown_name。
		if incomplete {
			out.Outcome, out.RemainingItems = relationQueryIncomplete, &remaining
			return out, nil
		}
		out.Outcome = relationQueryUnknownName
		out.UnknownName = in.Subject
		if len(subjects) > 0 {
			out.UnknownName = in.Object
		}
		return out, nil
	}

	// ⭐ 同名多人 → 要求澄清，**不挑一个**。
	// ⚠️ 挑一个的后果是把甲的事答成乙的，而用户完全看不出来。
	if len(subjects) > 1 || len(objects) > 1 {
		out.Outcome = relationQueryAmbiguous
		for i := range subjects {
			subjects[i].QueryRole = "subject"
		}
		for i := range objects {
			objects[i].QueryRole = "object"
		}
		out.Candidates = append(append([]relationCandidate{}, subjects...), objects...)
		if len(subjects) == 1 {
			out.Candidates = objects
		} else if len(objects) == 1 {
			out.Candidates = subjects
		}
		return out, nil
	}

	// ⭐ 两个名字指向同一个人：这本身就是答案。
	// ⚠️ 答成"没有关系"是错的——「阿Q 和 老Q 是什么关系」的正确答案
	// 是"他们是同一个人"。
	if subjects[0].ID == objects[0].ID {
		out.Outcome = relationQuerySameEntity
		out.Candidates = subjects
		return out, nil
	}

	rows, err := r.queries.FindRelationsBetweenCharacters(ctx,
		gen.FindRelationsBetweenCharactersParams{
			JobID:     jobID,
			Subjects:  []string{subjects[0].ID},
			Objects:   []string{objects[0].ID},
			Objects2:  []string{objects[0].ID},
			Subjects2: []string{subjects[0].ID},
			Limit:     maxRelationRecords + 1,
		})
	if err != nil {
		return out, fmt.Errorf("knowledge: find relations: %w", err)
	}
	if len(rows) > maxRelationRecords {
		// ⚠️ 截断必须**显式报告**，否则一个关系很多的人物对会安静地少返回
		// 几条，而回答看起来是完整的。
		rows, out.Truncated = rows[:maxRelationRecords], true
	}
	if len(rows) == 0 {
		if incomplete {
			out.Outcome, out.RemainingItems = relationQueryIncomplete, &remaining
			return out, nil
		}
		out.Outcome = relationQueryNoRecords
		return out, nil
	}

	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	evidence, err := r.queries.ListEvidenceForRelations(ctx, ids)
	if err != nil {
		return out, fmt.Errorf("knowledge: list relation evidence: %w", err)
	}
	byRelation := map[string][]relationEvidenceRecord{}
	for _, ev := range evidence {
		byRelation[ev.RelationID] = append(byRelation[ev.RelationID], relationEvidenceRecord{
			Quote: ev.Quote, SourceStart: int(ev.SourceStart), SourceEnd: int(ev.SourceEnd),
			SourceOrder: ev.SourceOrder, ChunkID: ev.ChunkID,
		})
	}
	for _, row := range rows {
		rec := relationRecord{
			Type: row.RelationType, IsDirected: row.IsDirected,
			SubjectName: row.SubjectName, ObjectName: row.ObjectName,
			FirstSourceOrder: row.FirstSourceOrder,
			Evidence:         byRelation[row.ID],
		}
		if row.ChapterNumber.Valid {
			n := int(row.ChapterNumber.Int32)
			rec.ChapterNumber = &n
		}
		if row.ChapterTitle.Valid {
			rec.ChapterTitle = &row.ChapterTitle.String
		}
		out.Relations = append(out.Relations, rec)
	}
	out.Outcome = relationQueryFound
	return out, nil
}

func (r *Repository) findCharacters(ctx context.Context, jobID, name, selectedID string) ([]relationCandidate, error) {
	if selectedID != "" {
		id, err := r.queries.GetNarrativeCharacterInJob(ctx, gen.GetNarrativeCharacterInJobParams{ID: selectedID, JobID: jobID})
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("knowledge: find selected character: %w", err)
		}
		return []relationCandidate{{ID: id, DisplayName: name}}, nil
	}
	rows, err := r.queries.FindCharactersByNameInJob(ctx, gen.FindCharactersByNameInJobParams{
		JobID: jobID, DisplayName: name, Surface: name, Limit: maxRelationCandidates,
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: find characters by name: %w", err)
	}
	out := make([]relationCandidate, 0, len(rows))
	for _, row := range rows {
		out = append(out, relationCandidate{
			ID: row.ID, DisplayName: row.DisplayName,
			FirstSourceOrder: row.FirstSourceOrder, HasAmbiguity: row.HasAmbiguity,
		})
	}
	return out, nil
}

// --- 对外契约（供 conversation 调用）---

// RelationAnswer 是一次关系查询的对外结果。
//
// ⚠️ Outcome 是**给调用方分支用的**，不是给用户看的文案。
// 文案由对话层决定，因为同一个结局在不同语境下要说的话不同。
type RelationAnswer struct {
	Outcome        string
	DocumentID     string
	Citations      []RelationCitation
	Candidates     []RelationCandidateInfo
	UnknownName    string
	RemainingItems *int
	Truncated      bool
}

// RelationCitation 是一条可以送进 prompt 的关系证据。
// ⚠️ 没有相似度字段，理由见 relation_citation.go。
type RelationCitation struct {
	RelationType string
	IsDirected   bool
	SubjectName  string
	ObjectName   string
	// DocumentName 由核验阶段从 PG 侧补上（MySQL 侧的关系记录里没有它）。
	DocumentName  string
	ChapterNumber *int
	Quote         string
	SourceOrder   int64
	SourceStart   int
	SourceEnd     int
	ChunkID       string
}

type RelationCandidateInfo struct {
	CharacterID      string
	DisplayName      string
	FirstSourceOrder int64
	HasAmbiguity     bool
	QueryRole        string
}

// 对外的结局常量。
const (
	RelationOutcomeFound        = string(relationQueryFound)
	RelationOutcomeNoRecords    = string(relationQueryNoRecords)
	RelationOutcomeIncomplete   = string(relationQueryIncomplete)
	RelationOutcomeNotExtracted = string(relationQueryNotExtracted)
	RelationOutcomeUnknownName  = string(relationQueryUnknownName)
	RelationOutcomeAmbiguous    = string(relationQueryAmbiguous)
	RelationOutcomeSameEntity   = string(relationQuerySameEntity)
	RelationOutcomeOutOfScope   = string(relationQueryOutOfScope)
	RelationOutcomeStale        = string(relationQueryStale)
)

// QueryRelations 回答「A 和 B 是什么关系」。
//
// ⚠️ documentIDs 必须是**调用方已经下推过 Agent 范围**的列表。
// 空列表表示"没有任何可查的文档"，不是"不限定"——本函数不会替调用方
// 去猜范围，因为猜错的表现是 Agent 悄悄用起范围外的资料。
//
// budgetRunes 是**既有 RAG 预算里分给关系证据的那一份**，不是另开的一份。
func (s *service) QueryRelations(ctx context.Context, documentIDs []string, subject, object string, budgetRunes int) (RelationAnswer, error) {
	return s.QueryRelationsSelected(ctx, documentIDs, subject, object, "", "", budgetRunes)
}

func (s *service) QueryRelationsSelected(ctx context.Context, documentIDs []string, subject, object, subjectID, objectID string, budgetRunes int) (RelationAnswer, error) {
	res, err := s.repo.queryRelations(ctx, relationQueryInput{
		DocumentIDs: documentIDs, Subject: subject, Object: object, SubjectID: subjectID, ObjectID: objectID,
	})
	if err != nil {
		return RelationAnswer{}, err
	}
	ans := RelationAnswer{
		Outcome: string(res.Outcome), DocumentID: res.DocumentID,
		UnknownName: res.UnknownName, RemainingItems: res.RemainingItems,
		Truncated: res.Truncated,
	}
	for _, c := range res.Candidates {
		ans.Candidates = append(ans.Candidates, RelationCandidateInfo{
			CharacterID: c.ID,
			DisplayName: c.DisplayName, FirstSourceOrder: c.FirstSourceOrder,
			HasAmbiguity: c.HasAmbiguity, QueryRole: c.QueryRole,
		})
	}
	if res.Outcome != relationQueryFound {
		return ans, nil
	}
	cites, truncated := selectRelationCitations(res.Relations, budgetRunes)
	ans.Truncated = ans.Truncated || truncated
	for _, c := range cites {
		ans.Citations = append(ans.Citations, RelationCitation{
			RelationType: c.RelationType, IsDirected: c.IsDirected,
			SubjectName: c.SubjectName, ObjectName: c.ObjectName,
			ChapterNumber: c.ChapterNumber, Quote: c.Quote,
			SourceOrder: c.SourceOrder, SourceStart: c.SourceStart,
			SourceEnd: c.SourceEnd, ChunkID: c.ChunkID,
		})
	}
	// ⭐ 核验：引用必须**现在**仍然指着当初那段字。见 relation_verify.go。
	chunks, err := s.repo.loadChunksForVerification(ctx,
		res.DocumentID, res.DocumentVersion, citationChunkIDs(ans.Citations))
	if err != nil {
		return RelationAnswer{}, err
	}
	kept, dropped := verifyCitationsAgainstChunks(ans.Citations, chunks)
	ans.Citations = kept
	// ⚠️ 核验刷掉了东西**必须**反映成"不完整"。默默少给几条，
	// 回答会以这几条为全部，说出"他们之间只有这些关系"。
	if dropped > 0 {
		ans.Truncated = true
	}

	if s.beforeRelationRecheck != nil {
		s.beforeRelationRecheck(ctx)
	}
	// ⭐ 复检放在最后：它要回答的是"从开始查到现在有没有变过"。
	if err := s.repo.recheckRelationScope(ctx, res.DocumentID, res.DocumentVersion, res.JobID); err != nil {
		if errors.Is(err, errRelationScopeChanged) {
			// ⚠️ 停止本轮，不降级答一个"没找到"——那会让用户以为
			// 书里没写，而真实情况是这份资料刚刚变过。
			return RelationAnswer{Outcome: RelationOutcomeStale, DocumentID: res.DocumentID}, nil
		}
		return RelationAnswer{}, err
	}

	// ⭐ 挑完之后一条都没剩下时，结局改成"截断"而不是留着 found。
	// ⚠️ 留着 found 而 Citations 为空，对话层会走"有证据"分支去做受限生成，
	// 而它手上一条证据都没有——模型只能编。
	if len(ans.Citations) == 0 {
		ans.Outcome = RelationOutcomeIncomplete
		ans.Truncated = true
	}
	return ans, nil
}

// --- 010 T035：对话里可以问关系的书目 ---

// RelationDocument 是一本"可以问人物关系的书"。
//
// ⚠️ 字段全部是**用户能看懂的话**：没有 epoch、没有 hash、没有 job_id。
// 这些是系统内部用来保证正确性的东西，对用户没有任何意义，
// 出现在界面上只会让人以为自己需要理解它们。
type RelationDocument struct {
	DocumentID string
	FileName   string
	// Ready 表示"现在问就能得到基于全书的答案"。
	// ⚠️ 它**不是** document.status == ready：抽取跑完才算，
	// 而抽取比解析晚得多。混同的表现是用户看到"已就绪"、
	// 问出来却是"还没跑完"。
	Ready bool
	// RemainingItems 是还没处理的片段数，Ready 时为 0。
	RemainingItems int
	// Stopped 表示这一轮已经停下来了（暂停、预算耗尽、失败）。
	// ⚠️ 与"还没跑完"分开：前者要用户去点续跑，后者只要等。
	Stopped bool
}

const maxRelationDocuments = 200

// listRelationDocuments 列出这些知识库里开启了关系抽取的文档。
func (r *Repository) listRelationDocuments(ctx context.Context, kbIDs []string) ([]RelationDocument, error) {
	if len(kbIDs) == 0 {
		// ⭐ 空范围不是全库，同 queryRelations。
		return nil, nil
	}
	rows, err := r.queries.ListRelationDocumentsInKnowledgeBases(ctx,
		gen.ListRelationDocumentsInKnowledgeBasesParams{
			KnowledgeBaseIds: kbIDs, Limit: maxRelationDocuments,
		})
	if err != nil {
		return nil, fmt.Errorf("knowledge: list relation documents: %w", err)
	}
	out := make([]RelationDocument, 0, len(rows))
	for _, row := range rows {
		doc := RelationDocument{DocumentID: row.ID, FileName: row.FileName}
		if !row.JobState.Valid {
			// 开关开着但作业行不在了——按"还没开始"报，不报就绪。
			out = append(out, doc)
			continue
		}
		total, done := int(row.TotalItems.Int32), int(row.SucceededItems.Int32)+int(row.FailedItems.Int32)
		doc.RemainingItems = total - done
		if doc.RemainingItems < 0 {
			doc.RemainingItems = 0
		}
		doc.Ready = row.InitializationComplete.Valid && row.InitializationComplete.Bool &&
			doc.RemainingItems == 0
		switch row.JobState.String {
		case jobStatePaused, jobStateBudgetExhausted, jobStateFailed:
			doc.Stopped = true
		}
		out = append(out, doc)
	}
	return out, nil
}

// ListRelationDocuments 是 conversation 拿"可以问关系的书目"的入口。
//
// ⚠️ kbIDs 必须是**调用方已经下推过 Agent 范围**的列表，同 QueryRelations。
// 空列表表示"这个 Agent 没挂任何知识库"，不是"不限定"。
func (s *service) ListRelationDocuments(ctx context.Context, kbIDs []string) ([]RelationDocument, error) {
	return s.repo.listRelationDocuments(ctx, kbIDs)
}
