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
	Relations  []relationDraft
}

type publishInput struct {
	JobID           string
	ItemID          string
	Epoch           int
	Outcome         extractionOutcome
	ExtractResponse []byte
	AliasResponse   []byte
}

// publishItemOutcome 在一个事务里写完全部结果。
func (r *Repository) publishItemOutcome(ctx context.Context, in publishInput) error {
	return platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)

		// 本地引用 -> 真实人物 ID。
		// ⚠️ 顺序遍历 Characters 而不是 range 一个 map：ID 是新生成的，
		// 但**生成顺序**决定了 first_source_order 相同时的排序结果，
		// map 迭代顺序会让同一份响应两次产出不同的记录顺序（宪法第 V 条）。
		ids := make(map[string]string, len(in.Outcome.Characters))
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
