package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"hify/internal/db/gen"
	"hify/internal/db/pggen"
	"hify/internal/platform/apperr"
)

// relations.go 是"这两个人是什么关系"的查询侧（010 T031/T032/T034）。
//
// ⭐ 整个功能的立论在这里体现：关系**不按当前状态覆盖写**，所以查询也不取
// "最新的那条"。"第 3 回是师徒、第 57 回反目"两条都要给出来——只给最后一个
// 状态，恰恰是这类问题最没用的答案。
//
// ⚠️ 这里绝不做三件事：
//  1. 不触发在线抽取——用户问一句话不该开始烧几小时 GPU;
//  2. 不回退到"让模型凭记忆答"——那样答案看起来一样好，但没有任何原文支持，
//     而这个功能的全部价值就是"每一条都能翻到原文";
//  3. 拿不准的时候不替用户选人。一个称呼命中多个实体就返回候选让他挑，
//     猜一个的代价是把甲的关系说成乙的，而回答本身读起来完全正常。

const (
	// maxRelationCandidates 是关系记录的候选上限（plan §7）。
	maxRelationCandidates = 200
	// maxRelationEvidenceItems 是最终送进模型的证据条数上限。
	maxRelationEvidenceItems = 12
	// maxSurfaceRunes 与契约一致：subject/object 非空且 ≤128 rune。
	maxSurfaceRunes = 128
	// maxRelationCharacterCandidates 是澄清时返回多少个候选。
	maxRelationCharacterCandidates = 8
)

// RelationQueryStatus 是一次查询的结局。
//
// ⚠️ 五个取值不能合并，尤其是 not_found 与 error：把读不到数据说成
// "没有找到关系"，用户会当成"书里确实没写"，而事实是我们没读出来。
type RelationQueryStatus string

const (
	RelationStatusFound      RelationQueryStatus = "found"
	RelationStatusNotFound   RelationQueryStatus = "not_found"
	RelationStatusAmbiguous  RelationQueryStatus = "ambiguous"
	RelationStatusDisabled   RelationQueryStatus = "disabled"
	RelationStatusIncomplete RelationQueryStatus = "incomplete"
	// RelationStatusSameEntity：两个称呼归一到同一个人。
	// ⭐ 单独一个状态而不是"没有关系"：后者会让用户以为书里没写，
	// 而事实是这两个称呼说的是同一个人。
	RelationStatusSameEntity RelationQueryStatus = "same_entity"
)

var (
	// ErrRelationScopeEmpty：调用方没有给任何知识库范围。
	//
	// ⭐ 空范围**拒绝**，绝不当成"全库"。当成全库的后果是一个没有配任何
	// 知识库的 Agent 能查到所有书——而每一层单独看都没有越权。
	ErrRelationScopeEmpty = apperr.InvalidInput("knowledge.relation_scope_empty",
		"当前助手没有配置可查询的知识库")

	// ErrRelationDocumentOutOfScope：文档不在调用方的范围内。
	// ⚠️ 报 404 语义（不存在），不报 403：403 会顺带确认这个文档是真的。
	ErrRelationDocumentOutOfScope = apperr.NotFound("knowledge.relation_document_out_of_scope",
		"指定的文档不在当前助手的可查询范围内")

	// ErrRelationQueryUnavailable：数据库/PG 读取失败。
	// ⚠️ 绝不映射成 not_found，理由见 RelationQueryStatus。
	ErrRelationQueryUnavailable = apperr.Conflict("knowledge.relation_query_unavailable",
		"关系记录暂时无法读取，请稍后重试")

	// ErrRelationEvidenceStale：入模前核验发现证据对不上当前原文。
	ErrRelationEvidenceStale = apperr.Conflict("knowledge.relation_evidence_stale",
		"文档内容已更新，关系依据需要重新抽取后才能引用")

	// ErrRelationSurfaceInvalid：称呼为空或过长。
	ErrRelationSurfaceInvalid = apperr.InvalidInput("knowledge.relation_surface_invalid",
		"人物称呼不能为空且不超过 128 个字符")
)

// RelationScope 是**服务端**建立的可查询范围。
//
// ⚠️ 它绝不能来自客户端。客户端能指定范围的话，"这个助手能查哪些书"
// 就变成了一个前端参数——而越权的请求和正常请求在服务端看来完全一样。
type RelationScope struct {
	KnowledgeBaseIDs []string
	// DocumentIDs 为空**只在 KB 范围非空时**表示"该范围内的全部文档"。
	DocumentIDs []string
}

// RelationQuery 是一次查询的输入。
type RelationQuery struct {
	Scope      RelationScope
	DocumentID string
	Subject    string
	Object     string
	// 两个 ID 是用户从歧义候选里选定的。⚠️ 服务端仍然要校验它们属于
	// 当前文档的当前 run——只凭 ID 就信，等于给了一条绕过范围校验的路。
	SubjectCharacterID string
	ObjectCharacterID  string
}

// RelationCharacterCandidate 是澄清时给用户看的候选。
type RelationCharacterCandidate struct {
	CharacterID string
	DisplayName string
	// Context 是这个候选的上下文称谓/出处，用来让用户分辨同名的两个人。
	Context      string
	HasAmbiguity bool
}

// RelationEvidenceItem 是一条可以翻回原文的依据。
type RelationEvidenceItem struct {
	ChunkID       string
	DocumentID    string
	DocumentName  string
	Quote         string
	SourceStart   int
	SourceEnd     int
	SourceOrder   int64
	ChapterNumber *int
	ChapterTitle  string
	PageNumber    *int
}

// RelationRecord 是一条关系记录（一个三元组在一处出处上的呈现）。
type RelationRecord struct {
	RelationID  string
	Type        string
	IsDirected  bool
	SubjectName string
	ObjectName  string
	// ⚠️ 章节是**首条证据的展示快照**，不是排序依据：倒叙的书里
	// 章节顺序 ≠ 故事顺序。排序用 SourceOrder（原文位置）。
	ChapterNumber *int
	ChapterTitle  string
	SourceOrder   int64
	Evidence      []RelationEvidenceItem
}

// RelationCoverage 说明"这本书抽到什么程度"，随每次回答一起展示。
//
// ⭐ 覆盖度不是可选的装饰：一次只跑完一半的抽取给出的关系是真的、但**不全**，
// 而用户会拿它当全部。不说清楚就等于让一个不完整的结果冒充完整的。
type RelationCoverage struct {
	State          string
	TotalItems     *int
	SucceededItems int
	FailedItems    int
	Complete       bool
}

// RelationQueryResult 是一次查询的全部结果。
type RelationQueryResult struct {
	Status            RelationQueryStatus
	Records           []RelationRecord
	SubjectCandidates []RelationCharacterCandidate
	ObjectCandidates  []RelationCharacterCandidate
	Coverage          RelationCoverage
	// HasMore 表示数据库层被 LIMIT 截断了。
	// ⚠️ 必须由 LIMIT+1 得出，不能靠"条数正好等于上限"去猜：
	// 正好这么多和被截断在结果里长得一模一样。
	HasMore bool
	// EvidenceTruncated 表示证据被选取上限裁掉了一部分。
	EvidenceTruncated bool
}

// QueryRelations 查两个称呼之间的关系。
func (s *service) QueryRelations(ctx context.Context, q RelationQuery) (RelationQueryResult, error) {
	if err := validateSurface(q.Subject); err != nil {
		return RelationQueryResult{}, err
	}
	if err := validateSurface(q.Object); err != nil {
		return RelationQueryResult{}, err
	}
	if len(q.Scope.KnowledgeBaseIDs) == 0 {
		return RelationQueryResult{}, ErrRelationScopeEmpty
	}

	doc, err := s.repo.getDocument(ctx, q.DocumentID)
	if err != nil {
		if errors.Is(err, ErrDocumentNotFound) {
			// 范围外和不存在对调用者是同一件事。
			return RelationQueryResult{}, ErrRelationDocumentOutOfScope
		}
		return RelationQueryResult{}, err
	}
	if !scopeAllows(q.Scope, doc) {
		return RelationQueryResult{}, ErrRelationDocumentOutOfScope
	}
	// ⚠️ 范围校验通过之前不读任何关系数据，也不在错误信息里带出文档名——
	// 否则一次越权尝试能拿到"这本书存在、进度多少"这类信息。
	if !doc.IsRelationExtractionEnabled || doc.ActiveRelationJobID == "" {
		return RelationQueryResult{Status: RelationStatusDisabled}, nil
	}
	if doc.Status != StatusReady {
		return RelationQueryResult{Status: RelationStatusIncomplete}, nil
	}

	status, err := s.repo.extractionStatus(ctx, doc)
	if err != nil {
		return RelationQueryResult{}, ErrRelationQueryUnavailable
	}
	coverage := RelationCoverage{
		State: status.State, TotalItems: status.TotalItems,
		SucceededItems: status.SucceededItems, FailedItems: status.FailedItems,
		Complete: status.State == jobStateSucceeded && status.FailedItems == 0,
	}
	if status.DocumentVersion != doc.Version {
		// 作业挂在旧版本上：它的关系指向的原文已经不是用户现在看到的了。
		return RelationQueryResult{Status: RelationStatusIncomplete, Coverage: coverage}, nil
	}

	subject, subjectCands, err := s.resolveCharacter(ctx, doc.ActiveRelationJobID, q.Subject, q.SubjectCharacterID)
	if err != nil {
		return RelationQueryResult{}, err
	}
	object, objectCands, err := s.resolveCharacter(ctx, doc.ActiveRelationJobID, q.Object, q.ObjectCharacterID)
	if err != nil {
		return RelationQueryResult{}, err
	}
	if subject == "" || object == "" {
		// 至少一侧要澄清。⚠️ 两侧的候选都带上：只回一侧的话，用户选完
		// 还要再被问一次，而第二次问的时候他已经不知道自己在选什么了。
		return RelationQueryResult{
			Status: RelationStatusAmbiguous, Coverage: coverage,
			SubjectCandidates: subjectCands, ObjectCandidates: objectCands,
		}, nil
	}
	if subject == object {
		return RelationQueryResult{Status: RelationStatusSameEntity, Coverage: coverage}, nil
	}

	records, hasMore, err := s.repo.relationsBetween(ctx, doc, subject, object)
	if err != nil {
		return RelationQueryResult{}, ErrRelationQueryUnavailable
	}
	if len(records) == 0 {
		return RelationQueryResult{Status: RelationStatusNotFound, Coverage: coverage}, nil
	}

	selected, truncated := selectRelationEvidence(records, maxRelationEvidenceItems)
	if err := s.repo.verifyRelationEvidence(ctx, doc, selected); err != nil {
		return RelationQueryResult{}, err
	}
	return RelationQueryResult{
		Status: RelationStatusFound, Records: selected, Coverage: coverage,
		HasMore: hasMore, EvidenceTruncated: truncated,
	}, nil
}

func validateSurface(s string) error {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > maxSurfaceRunes {
		return ErrRelationSurfaceInvalid
	}
	return nil
}

// scopeAllows 是范围校验：文档必须在 KB 范围内；DocumentIDs 非空时还必须
// 在那个列表里。
//
// ⚠️ DocumentIDs 为空表示"该 KB 范围内的全部文档"，**而 KB 范围为空已经在
// 上面被拒了**。两者的区别是这个功能最容易出错的地方：把"空 = 全部"用在
// KB 上，一个没配知识库的助手就能查到所有书。
func scopeAllows(scope RelationScope, doc Document) bool {
	inKB := false
	for _, id := range scope.KnowledgeBaseIDs {
		if id == doc.KnowledgeBaseID {
			inKB = true
			break
		}
	}
	if !inKB {
		return false
	}
	if len(scope.DocumentIDs) == 0 {
		return true
	}
	for _, id := range scope.DocumentIDs {
		if id == doc.ID {
			return true
		}
	}
	return false
}

// resolveCharacter 把一个称呼解析成一个人物 ID。
//
// 返回空 ID 且无错误 = 需要澄清（候选在第二个返回值里）。
//
// ⭐ "只有唯一一个 supported 候选才算解析成功"是这个函数的全部规则。
// 命中两个就澄清，命中一个但它被标成歧义也澄清——替用户选一个的代价是
// 把甲的关系说成乙的，而回答读起来完全正常。
func (s *service) resolveCharacter(ctx context.Context, jobID, surface, pinnedID string) (string, []RelationCharacterCandidate, error) {
	cands, err := s.repo.charactersBySurface(ctx, jobID, surface)
	if err != nil {
		return "", nil, ErrRelationQueryUnavailable
	}
	if pinnedID != "" {
		// ⚠️ 用户选定的 ID 必须是**这次解析出来的候选之一**：
		// 只校验"这个 ID 属于本作业"还不够，那样可以拿一个与本次称呼
		// 毫无关系的人物 ID 进来，而它确实属于本作业。
		for _, c := range cands {
			if c.CharacterID == pinnedID {
				return pinnedID, nil, nil
			}
		}
		return "", cands, nil
	}
	if len(cands) != 1 {
		return "", cands, nil
	}
	if cands[0].HasAmbiguity {
		return "", cands, nil
	}
	ambiguous, err := s.repo.surfaceIsAmbiguous(ctx, jobID, surface)
	if err != nil {
		return "", nil, ErrRelationQueryUnavailable
	}
	if ambiguous {
		// 这个称呼在别的地方被判过歧义：即使这里只命中一个人物，
		// 也不能当成确定的。
		return "", cands, nil
	}
	return cands[0].CharacterID, nil, nil
}

// --- 证据选取（T032）---

// selectRelationEvidence 从候选记录里选出最终送进模型的证据。
//
// ⭐ 选取顺序是契约定死的，不是随手排的：
//  1. 每一个不同的 (类型, 方向) 各留**最早**的一条证据——先保证"有几种
//     关系"这件事不被同一种关系的大量证据挤掉;
//  2. 再按原文顺序补各章的证据。
//
// ⚠️ 排序一律用 SourceOrder（原文位置），不用章节号：倒叙的书里章节顺序
// 不等于故事顺序，按章节号排会把"后来发生的事"排到前面。
func selectRelationEvidence(records []RelationRecord, limit int) ([]RelationRecord, bool) {
	type pick struct {
		recordIdx int
		evidence  RelationEvidenceItem
	}
	var first []pick
	var rest []pick
	seenKind := map[string]bool{}
	for i, rec := range records {
		kind := rec.Type + "|" + boolKey(rec.IsDirected) + "|" + rec.SubjectName + "->" + rec.ObjectName
		for j, ev := range rec.Evidence {
			if j == 0 && !seenKind[kind] {
				seenKind[kind] = true
				first = append(first, pick{i, ev})
				continue
			}
			rest = append(rest, pick{i, ev})
		}
	}
	sortPicks := func(ps []pick) {
		sort.SliceStable(ps, func(a, b int) bool {
			if ps[a].evidence.SourceOrder != ps[b].evidence.SourceOrder {
				return ps[a].evidence.SourceOrder < ps[b].evidence.SourceOrder
			}
			return ps[a].evidence.SourceStart < ps[b].evidence.SourceStart
		})
	}
	sortPicks(first)
	sortPicks(rest)

	kept := make(map[int][]RelationEvidenceItem, len(records))
	total := 0
	truncated := false
	for _, group := range [][]pick{first, rest} {
		for _, p := range group {
			if total >= limit {
				truncated = true
				break
			}
			kept[p.recordIdx] = append(kept[p.recordIdx], p.evidence)
			total++
		}
	}

	out := make([]RelationRecord, 0, len(records))
	for i, rec := range records {
		evidence := kept[i]
		if len(evidence) == 0 {
			// ⚠️ 一条没有任何证据入选的关系**不进结果**：把它留下来
			// 等于给出一个"书里有这条关系"的断言却拿不出原文，
			// 而这正是整个功能要避免的东西。
			truncated = true
			continue
		}
		sort.SliceStable(evidence, func(a, b int) bool {
			return evidence[a].SourceOrder < evidence[b].SourceOrder
		})
		rec.Evidence = evidence
		out = append(out, rec)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].SourceOrder != out[b].SourceOrder {
			return out[a].SourceOrder < out[b].SourceOrder
		}
		return out[a].RelationID < out[b].RelationID
	})
	return out, truncated
}

func boolKey(b bool) string {
	if b {
		return "d"
	}
	return "u"
}

// --- 仓储侧 ---

func (r *Repository) charactersBySurface(ctx context.Context, jobID, surface string) ([]RelationCharacterCandidate, error) {
	sum := sha256.Sum256([]byte(surface))
	rows, err := r.queries.FindCharactersBySurface(ctx, gen.FindCharactersBySurfaceParams{
		JobID: jobID, DisplayName: surface,
		JobID_2: jobID, SurfaceHash: sum[:],
		Limit: maxRelationCharacterCandidates,
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: find characters by surface: %w", err)
	}
	out := make([]RelationCharacterCandidate, 0, len(rows))
	for _, row := range rows {
		out = append(out, RelationCharacterCandidate{
			CharacterID: row.ID, DisplayName: row.DisplayName,
			HasAmbiguity: row.HasAmbiguity,
			// 上下文用"第一次出现的位置"表述，让用户能分辨同名的两个人。
			// ⚠️ 不暴露 source_order 这个内部数字本身。
			Context: fmt.Sprintf("首次出现于第 %d 个片段", row.FirstSourceOrder+1),
		})
	}
	return out, nil
}

func (r *Repository) surfaceIsAmbiguous(ctx context.Context, jobID, surface string) (bool, error) {
	sum := sha256.Sum256([]byte(surface))
	n, err := r.queries.CountAmbiguousAliasesBySurface(ctx,
		gen.CountAmbiguousAliasesBySurfaceParams{JobID: jobID, SurfaceHash: sum[:]})
	if err != nil {
		return false, fmt.Errorf("knowledge: count ambiguous aliases: %w", err)
	}
	return n > 0, nil
}

// relationsBetween 读两个人物之间的全部关系及其证据。
func (r *Repository) relationsBetween(ctx context.Context, doc Document, subjectID, objectID string) ([]RelationRecord, bool, error) {
	rows, err := r.queries.ListRelationsBetweenCharacters(ctx, gen.ListRelationsBetweenCharactersParams{
		JobID:       doc.ActiveRelationJobID,
		SubjectID:   subjectID,
		ObjectID:    objectID,
		SubjectID_2: objectID,
		ObjectID_2:  subjectID,
		// ⚠️ LIMIT+1：多出来的那一条不返回，只用来说明"还有更多"。
		Limit: maxRelationCandidates + 1,
	})
	if err != nil {
		return nil, false, fmt.Errorf("knowledge: list relations: %w", err)
	}
	hasMore := len(rows) > maxRelationCandidates
	if hasMore {
		rows = rows[:maxRelationCandidates]
	}
	if len(rows) == 0 {
		return nil, false, nil
	}

	ids := make([]string, 0, len(rows))
	byID := make(map[string]int, len(rows))
	records := make([]RelationRecord, 0, len(rows))
	for i, row := range rows {
		ids = append(ids, row.ID)
		byID[row.ID] = i
		records = append(records, RelationRecord{
			RelationID: row.ID, Type: row.RelationType, IsDirected: row.IsDirected,
			SubjectName: row.SubjectName, ObjectName: row.ObjectName,
			ChapterNumber: fromNullInt32(row.ChapterNumber),
			ChapterTitle:  row.ChapterTitle.String,
			SourceOrder:   row.FirstSourceOrder,
		})
	}

	evRows, err := r.queries.ListEvidenceForRelations(ctx, gen.ListEvidenceForRelationsParams{
		JobID: doc.ActiveRelationJobID, RelationIds: ids,
		Limit: int32(maxRelationCandidates) * maxRelationEvidenceItems,
	})
	if err != nil {
		return nil, false, fmt.Errorf("knowledge: list relation evidence: %w", err)
	}
	for _, ev := range evRows {
		idx, ok := byID[ev.RelationID]
		if !ok {
			continue
		}
		records[idx].Evidence = append(records[idx].Evidence, RelationEvidenceItem{
			ChunkID: ev.ChunkID, DocumentID: doc.ID, DocumentName: doc.FileName,
			Quote: ev.Quote, SourceStart: int(ev.SourceStart), SourceEnd: int(ev.SourceEnd),
			SourceOrder:   ev.SourceOrder,
			ChapterNumber: records[idx].ChapterNumber,
			ChapterTitle:  records[idx].ChapterTitle,
		})
	}
	return records, hasMore, nil
}

// verifyRelationEvidence 在入模之前逐条核对证据仍然对得上当前原文（T034）。
//
// ⭐ 这一步不是多余的：关系是抽取那一刻的快照，而文档可以在之后被重新处理。
// 引用一段**已经不在书里**的原文，比答不上来糟糕得多——用户会去翻原文，
// 翻不到，然后不再相信这个功能的任何一条引用。
//
// ⚠️ 任一条对不上就放弃整轮，不是"把对不上的那条剔掉再答"：剔掉之后剩下的
// 结论仍然建立在一个已经变了的语料上，而回答里没有任何东西说明这件事。
func (r *Repository) verifyRelationEvidence(ctx context.Context, doc Document, records []RelationRecord) error {
	ids := map[string]struct{}{}
	var list []string
	for _, rec := range records {
		for _, ev := range rec.Evidence {
			if _, seen := ids[ev.ChunkID]; seen {
				continue
			}
			ids[ev.ChunkID] = struct{}{}
			list = append(list, ev.ChunkID)
		}
	}
	if len(list) == 0 {
		return nil
	}
	// ⚠️ 批量读，不逐条：一次回答最多 12 条证据，逐条查就是 12 次往返。
	rows, err := r.pgQueries.GetPublishedNarrativeChunksByIDs(ctx,
		pggen.GetPublishedNarrativeChunksByIDsParams{
			DocumentID: doc.ID, DocumentVersion: doc.Version, Column3: list,
		})
	if err != nil {
		return ErrRelationQueryUnavailable
	}
	content := make(map[string]string, len(rows))
	for _, row := range rows {
		content[row.ID] = row.Content
	}
	for _, rec := range records {
		for _, ev := range rec.Evidence {
			body, ok := content[ev.ChunkID]
			if !ok {
				// 片段不在当前已发布版本里了。
				return ErrRelationEvidenceStale
			}
			if !strings.Contains(body, ev.Quote) {
				// 片段还在，但那句话已经不在它里面了。
				return ErrRelationEvidenceStale
			}
		}
	}
	return nil
}

func fromNullInt32(v sql.NullInt32) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int32)
	return &n
}
