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
}

type relationCandidate struct {
	ID               string
	DisplayName      string
	FirstSourceOrder int64
	HasAmbiguity     bool
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
	Relations  []relationRecord
	Candidates []relationCandidate
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

	// ⭐ 只读文档**当前指向**的那个 run。⚠️ 读到旧 run 的记录，用户会看到
	// 自己已经"重新开始"过的那一次的结果，而界面显示的是新 run 的进度。
	job, err := r.queries.GetRelationExtractionJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return out, fmt.Errorf("knowledge: load job for relation query: %w", err)
	}

	subjects, err := r.findCharacters(ctx, jobID, in.Subject)
	if err != nil {
		return out, err
	}
	objects, err := r.findCharacters(ctx, jobID, in.Object)
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

func (r *Repository) findCharacters(ctx context.Context, jobID, name string) ([]relationCandidate, error) {
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
