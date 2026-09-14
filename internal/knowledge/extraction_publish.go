package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"hify/internal/db/gen"
	"hify/internal/platform"
	"hify/internal/platform/apperr"
)

// extraction_publish.go 把一个 item 的成功结果落库（010 T020）。
//
// ⭐ 人物、关系、证据、item 状态、作业计数在**同一个事务**里。分开的后果
// 不是"数据不一致"这种抽象说法，而是很具体的两种：
//   - 关系写了、item 没标成功 → 恢复后重跑，同一批关系再抽一遍，钱花两遍；
//   - item 标了成功、关系没写 → 覆盖率分子照涨，而那一段书其实是空的。
// 两者都不报错。

var (
	// ErrItemAlreadyPublished：这个 item 已经发布过。
	// ⚠️ 重复投递（asynq 重发、提交后丢 ACK）走到这里是**正常**的，
	// 调用方应当当成成功处理掉、ACK 掉，而不是当成错误重试。
	ErrItemAlreadyPublished = apperr.Conflict(
		"knowledge.item_already_published", "该片段的抽取结果已经发布过")

	// ErrExtractionEpochLost：发布时发现自己已经不是作业持有者。
	// ⚠️ 此刻手上那份结果算的是**旧版本**语料，必须整体丢弃。
	ErrExtractionEpochLost = apperr.Conflict(
		"knowledge.extraction_epoch_lost", "该抽取作业已被其他任务接管")
)

// characterDraft 是本次抽取里新建的一个人物。
// LocalRef 是模型在**本次响应内**用的临时引用，不出数据库。
type characterDraft struct {
	LocalRef         string
	DisplayName      string
	FirstSourceOrder int64
	IdentityEvidence []byte
	HasAmbiguity     bool
	// Aliases 是这个人物在本块里用到的**全部称呼**，DisplayName 只是其中
	// 第一个。
	//
	// ⭐ 不存它们，查询就只能按正名命中：用户问"老Q和赵太爷什么关系"，
	// 而库里那个人物叫"阿Q"——归一明明成功了，查询却说没找到，
	// 且没有任何线索说明为什么。
	Aliases []aliasDraft
}

// aliasDraft 是一个称呼到人物的映射。
// ⚠️ State 只有 supported 才允许在查询时命中：proposed/ambiguous 指向的是
// "我们还没敢下结论"，拿它当命中等于用一个未定的判断回答用户。
type aliasDraft struct {
	Surface  string
	State    string
	Evidence []byte
}

type evidenceDraft struct {
	ChunkID         string
	DocumentVersion int64
	SourceOrder     int64
	SourceStart     int
	SourceEnd       int
	Quote           string
	SourceSegments  []byte
}

type relationDraft struct {
	SubjectRef       string
	ObjectRef        string
	Type             string
	IsDirected       bool
	FirstSourceOrder int64
	ChapterNumber    *int
	ChapterTitle     *string
	Evidence         []evidenceDraft
}

// extractionOutcome 是一个 item 处理完之后要落库的全部内容。
// ⚠️ 全空是**合法的成功结果**：一个块里没有关系是正常的。
type extractionOutcome struct {
	Characters []characterDraft
	// Existing 是归一阶段决定链接到的**既有**人物：组号 -> character_id。
	// ⚠️ 与 Characters 分开而不是塞一个"已存在"标志：这些 ID 不是这次
	// 创建的，误当成新人物插一遍会撞主键（好的情况），或者在幂等重放里
	// 悄悄多出一个同名人物（坏的情况）。
	Existing  map[string]string
	Relations []relationDraft
}

type publishInput struct {
	JobID           string
	ItemID          string
	Epoch           int
	Outcome         extractionOutcome
	ExtractResponse []byte
	AliasResponse   []byte
	// AliasDegraded：归一失败，这一块退回"每个称呼各自独立成人物"发布。
	// ⚠️ 必须跟着 item 一起落库，理由见 000018 迁移的注释：
	// 只写日志的降级就是静默降级——指标已经变了，而报告读不出来。
	AliasDegraded bool
}

// publishItemOutcome 在一个事务里写完全部结果。
func (r *Repository) publishItemOutcome(ctx context.Context, in publishInput) error {
	// 先只读取 document_id，真正的有效性判断和锁都在下面事务中完成。
	// job 的 document_id 在创建后不可变；拿它只是为了遵守 document → job →
	// item 的锁顺序，不能拿这次无锁读取当作当前性判定。
	job, err := r.queries.GetRelationExtractionJob(ctx, in.JobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrExtractionEpochLost
		}
		return fmt.Errorf("knowledge: load job before publish: %w", err)
	}
	return platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)
		// 锁顺序固定为 document → job → item。删除或 restart 与这一把 document
		// 锁串行：模型响应晚到时不会在删除/替换之后重新把关系写回来。
		doc, err := q.LockDocumentExtractionState(ctx, job.DocumentID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrExtractionEpochLost
			}
			return fmt.Errorf("knowledge: lock document before publish: %w", err)
		}
		if doc.Status != StatusReady || doc.Version != int64(job.DocumentVersion) ||
			!doc.ActiveRelationJobID.Valid || doc.ActiveRelationJobID.String != in.JobID {
			return ErrExtractionEpochLost
		}
		lockedJob, err := q.LockRelationExtractionJob(ctx, in.JobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrExtractionEpochLost
			}
			return fmt.Errorf("knowledge: lock job before publish: %w", err)
		}
		if lockedJob.State != jobStateRunning {
			return ErrExtractionEpochLost
		}
		lockedItem, err := q.LockRelationExtractionItem(ctx, gen.LockRelationExtractionItemParams{
			ID: in.ItemID, JobID: in.JobID,
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrExtractionEpochLost
			}
			return fmt.Errorf("knowledge: lock item before publish: %w", err)
		}
		if lockedItem == itemStateSucceeded {
			return ErrItemAlreadyPublished
		}

		// 本地引用 -> 真实人物 ID。
		// ⚠️ 顺序遍历 Characters 而不是 range 一个 map：ID 是新生成的，
		// 但**生成顺序**决定了 first_source_order 相同时的排序结果，
		// map 迭代顺序会让同一份响应两次产出不同的记录顺序（宪法第 V 条）。
		ids := make(map[string]string, len(in.Outcome.Characters)+len(in.Outcome.Existing))
		for group, characterID := range in.Outcome.Existing {
			// 只读的映射，不建人物。map 迭代顺序在这里无所谓：
			// 下面写库的顺序由 Characters / Relations 的切片顺序决定。
			ids[group] = characterID
		}
		for _, c := range in.Outcome.Characters {
			id := platform.NewID()
			ids[c.LocalRef] = id
			if err := q.CreateNarrativeCharacter(ctx, gen.CreateNarrativeCharacterParams{
				ID: id, JobID: in.JobID, DisplayName: c.DisplayName,
				FirstSourceOrder: c.FirstSourceOrder,
				IdentityEvidence: jsonOrNull(c.IdentityEvidence),
				HasAmbiguity:     c.HasAmbiguity,
			}); err != nil {
				return fmt.Errorf("knowledge: create character: %w", err)
			}
			for _, alias := range c.Aliases {
				surfaceHash := sha256.Sum256([]byte(alias.Surface))
				decisionKey := aliasDecisionKeyHash(in.JobID, id, alias)
				if err := q.UpsertNarrativeAlias(ctx, gen.UpsertNarrativeAliasParams{
					ID: platform.NewID(), JobID: in.JobID,
					CharacterID: sql.NullString{String: id, Valid: alias.State == aliasStateSupported},
					Surface:     alias.Surface, SurfaceHash: surfaceHash[:],
					State:    alias.State,
					Evidence: jsonOrEmptyArray(alias.Evidence),
					// 与人物同一个首次出现位置：别名不是独立实体，
					// 它的"第一次出现"说的是这个人物在本块里的位置。
					FirstSourceOrder: c.FirstSourceOrder,
					DecisionKeyHash:  decisionKey,
				}); err != nil {
					return fmt.Errorf("knowledge: create alias: %w", err)
				}
			}
		}

		for _, rel := range in.Outcome.Relations {
			subjectID, ok := ids[rel.SubjectRef]
			if !ok {
				// ⚠️ 悬空端点必须让整个 item 失败，不能跳过这条关系。
				// 跳过的表现是关系总数悄悄变小，而那是要进报告的数字。
				return fmt.Errorf("knowledge: relation subject %q not among created characters", rel.SubjectRef)
			}
			objectID, ok := ids[rel.ObjectRef]
			if !ok {
				return fmt.Errorf("knowledge: relation object %q not among created characters", rel.ObjectRef)
			}
			key := relationKeyHash(subjectID, objectID, rel.Type, rel.IsDirected, rel.FirstSourceOrder)
			if err := q.UpsertNarrativeRelation(ctx, gen.UpsertNarrativeRelationParams{
				ID: platform.NewID(), JobID: in.JobID,
				SubjectID: subjectID, ObjectID: objectID,
				RelationType: rel.Type, IsDirected: rel.IsDirected,
				RelationKeyHash: key, FirstSourceOrder: rel.FirstSourceOrder,
				ChapterNumber: nullInt32Ptr(rel.ChapterNumber),
				ChapterTitle:  nullStringPtr(rel.ChapterTitle),
			}); err != nil {
				return fmt.Errorf("knowledge: upsert relation: %w", err)
			}
			relationID, err := q.GetNarrativeRelationByKey(ctx, gen.GetNarrativeRelationByKeyParams{
				JobID: in.JobID, RelationKeyHash: key,
			})
			if err != nil {
				return fmt.Errorf("knowledge: reload relation: %w", err)
			}
			for _, ev := range rel.Evidence {
				if err := q.UpsertNarrativeRelationEvidence(ctx, gen.UpsertNarrativeRelationEvidenceParams{
					ID: platform.NewID(), JobID: in.JobID, RelationID: relationID,
					ChunkID: ev.ChunkID, DocumentVersion: int32(ev.DocumentVersion),
					SourceOrder: ev.SourceOrder,
					SourceStart: int32(ev.SourceStart), SourceEnd: int32(ev.SourceEnd),
					Quote: ev.Quote, SourceSegments: jsonOrNull(ev.SourceSegments),
					EvidenceKeyHash: evidenceKeyHash(ev),
				}); err != nil {
					return fmt.Errorf("knowledge: upsert evidence: %w", err)
				}
			}
		}

		n, err := q.MarkItemSucceeded(ctx, gen.MarkItemSucceededParams{
			ExtractResponse: jsonOrNull(in.ExtractResponse),
			AliasResponse:   jsonOrNull(in.AliasResponse),
			AliasDegraded:   in.AliasDegraded,
			ID:              in.ItemID, JobID: in.JobID,
		})
		if err != nil {
			return fmt.Errorf("knowledge: mark item succeeded: %w", err)
		}
		if n == 0 {
			return ErrItemAlreadyPublished
		}
		m, err := q.BumpJobItemOutcome(ctx, gen.BumpJobItemOutcomeParams{
			SucceededItems: 1, FailedItems: 0, ID: in.JobID, Epoch: int32(in.Epoch),
		})
		if err != nil {
			return fmt.Errorf("knowledge: bump item outcome: %w", err)
		}
		if m == 0 {
			// 自己已经不是持有者：整个事务回滚。
			return ErrExtractionEpochLost
		}
		return nil
	})
}

// relationKeyHash 算关系的去重键。
//
// ⭐ 无向关系**按实体 ID 排序后**再算，否则 (A,B) 与 (B,A) 会成为两条，
// 「同乡」这类关系被记两遍会直接虚增关系总数——那是要进报告的数字。
// 有向关系保留方向：「甲杀害乙」和「乙杀害甲」不是一回事。
//
// ⚠️ first_source_order 参与计算，所以**不同出处是不同的记录**。
// 这正是关系历史不被覆盖的实现方式：第 3 回师徒、第 57 回反目，两条都在。
func relationKeyHash(subjectID, objectID, relType string, directed bool, sourceOrder int64) []byte {
	a, b := subjectID, objectID
	if !directed {
		if a > b {
			a, b = b, a
		}
	}
	h := sha256.New()
	writeHashField(h, a)
	writeHashField(h, b)
	writeHashField(h, relType)
	if directed {
		writeHashField(h, "directed")
	} else {
		writeHashField(h, "undirected")
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(sourceOrder))
	h.Write(buf[:])
	return h.Sum(nil)
}

// evidenceKeyHash 算证据的去重键。
//
// ⭐ **不含 chunk_id**：相邻 chunk 因 overlap 会包含同一段原文，
// 按 chunk_id 去重会把同一处出处记成两条证据，虚增报告里的证据条数。
// 身份由「规范源区间 + 引文」决定，那才是"这句话在书里的哪个位置"。
func evidenceKeyHash(ev evidenceDraft) []byte {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(ev.SourceStart))
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], uint64(ev.SourceEnd))
	h.Write(buf[:])
	writeHashField(h, ev.Quote)
	return h.Sum(nil)
}

// writeHashField 写一个带长度前缀的字段。
// ⚠️ 长度前缀不可省：直接拼接会让 ("ab","c") 和 ("a","bc") 撞成同一个键。
func writeHashField(h interface{ Write([]byte) (int, error) }, s string) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(len(s)))
	h.Write(buf[:])
	h.Write([]byte(s))
}

// replayableResponse 是一次已经落盘、可以直接接着算的响应。
type replayableResponse struct {
	AttemptID    string
	Body         string
	FinishReason string
}

// findReplayableResponse 找这个 item 这个阶段已经落盘的成功响应。
//
// ⭐ 有的话，恢复的 worker 必须拿它接着算，**不能再打一次模型**。
// 再打一次的后果不是"结果不一致"，是那笔钱白花第二遍，而账目上看起来
// 完全正常——两次都是真实发生的调用。
//
// ⚠️ 只有 completed 且落了盘的才可回放。unknown 的不行：那次调用可能
// 根本没产生响应，拿它当结果就是凭空造数据。查询里的 state='completed'
// 就是这条边界。
func (r *Repository) findReplayableResponse(ctx context.Context, itemID, phase string) (replayableResponse, bool, error) {
	row, err := r.queries.FindReplayableAttempt(ctx, gen.FindReplayableAttemptParams{
		ItemID: itemID, Phase: phase,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return replayableResponse{}, false, nil
		}
		return replayableResponse{}, false, fmt.Errorf("knowledge: find replayable attempt: %w", err)
	}
	return replayableResponse{
		AttemptID: row.ID, Body: row.RawResponse.String,
		FinishReason: row.FinishReason.String,
	}, true, nil
}

// jsonOrNull 让空切片写成 SQL NULL 而不是零长度 JSON。
//
// ⚠️ 零长度值不是合法 JSON，MySQL 会拒绝；而更要紧的是语义：
// 这几列的 NULL 表示"没有这一段"（比如空结果的 item 没有 alias_response），
// 写成 "" 或 "{}" 会让"没有"和"有但是空的"变得无法区分。
const (
	aliasStateSupported = "supported"
	aliasStateAmbiguous = "ambiguous"
)

// aliasDecisionKeyHash 把"哪个作业、哪个人物、哪个称呼、什么结论"算成一个键。
//
// ⭐ 去重键**必须**包含人物与结论，不能只由 surface 组成：只按 surface 去重
// 就是在按名字合并人，正是这张表要防的事（000017 的注释记着同一件事）。
func aliasDecisionKeyHash(jobID, characterID string, alias aliasDraft) []byte {
	h := sha256.New()
	writeHashField(h, jobID)
	writeHashField(h, characterID)
	writeHashField(h, alias.Surface)
	writeHashField(h, alias.State)
	return h.Sum(nil)
}

// jsonOrEmptyArray：evidence 列是 NOT NULL 的，没有依据时写 []。
// ⚠️ 不写 null 也不写 {}：读的一侧永远拿到数组，少一处分支就少一个
// "有时候是 null" 的坑。
func jsonOrEmptyArray(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("[]")
	}
	return json.RawMessage(b)
}

func jsonOrNull(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

func nullInt32Ptr(v *int) sql.NullInt32 {
	if v == nil {
		return sql.NullInt32{}
	}
	return sql.NullInt32{Int32: int32(*v), Valid: true}
}

func nullStringPtr(v *string) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *v, Valid: true}
}
