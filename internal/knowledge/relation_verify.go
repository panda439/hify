package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"hify/internal/db/pggen"
)

// relation_verify.go 是引用在**入模之前**的最后一道核验（010 T034）。
//
// ⭐ 立论：关系记录写在 MySQL 里，它引用的原文却在 PG 里，两边没有外键、
// 也不在同一个事务里。抽取那一刻两边是对得上的，但从那以后文档可以被重新
// 处理、重新分块、删掉重传——**MySQL 侧的记录不会因此失效**。
//
// ⚠️ 不核验的后果不是报错，而是**引用悄悄指向别的文字**：块被重新切过
// 之后，同一个 chunk_id 上的同一段 rune 区间落在完全不同的一句话上。
// 用户点开引用看到的原文是真的，与回答里那句"证据"却毫无关系，
// 而系统的每一层都认为自己工作正常。
//
// 两道检查，顺序不可颠倒：
//  1. **PG 批量核验**：证据记的文档区间映射回块里，与 quote 逐字比对；
//  2. **MySQL 复检**：紧贴入模之前再读一次文档，确认版本没变、
//     当前指向的 run 还是这一轮读到的那个。
//
// ⚠️ 复检必须放在最后：它要回答的是"从开始查到现在有没有变过"，
// 提前做等于把窗口留在自己身后。

// maxVerifyChunkIDs 是一次批量核验的 chunk 上限。
//
// ⚠️ 这是给 PG 的 ANY($3::text[]) 收口用的。引用条数由
// maxRelationCitations（12）先卡了一道，这里是第二道——上游哪天放宽了
// 上限，也不至于一次把上千个 id 塞进一条 SQL。
const maxVerifyChunkIDs = 200

// verifyChunk 是核验需要的那部分块内容。
type verifyChunk struct {
	Content      string
	DocumentName string
	Meta         *narrativeMetadata
}

// loadChunksForVerification 批量取回引用涉及的已发布片段。
func (r *Repository) loadChunksForVerification(ctx context.Context, docID string, version int64, ids []string) (map[string]verifyChunk, error) {
	if len(ids) == 0 {
		return map[string]verifyChunk{}, nil
	}
	if len(ids) > maxVerifyChunkIDs {
		return nil, fmt.Errorf("knowledge: verify citations: %d chunk ids exceeds cap %d", len(ids), maxVerifyChunkIDs)
	}
	rows, err := r.pgQueries.GetPublishedNarrativeChunksByIDs(ctx, pggen.GetPublishedNarrativeChunksByIDsParams{
		DocumentID: docID, DocumentVersion: version, Column3: ids,
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: load chunks for citation verification: %w", err)
	}
	out := make(map[string]verifyChunk, len(rows))
	for _, row := range rows {
		meta, err := decodeNarrativeMetadata(row.NarrativeMetadata)
		if err != nil {
			// ⚠️ 元数据坏掉不是"这条引用不合格"，是数据本身出了问题——
			// 静默丢掉会让一次坏掉的写入表现为"关系变少了"。
			return nil, fmt.Errorf("knowledge: decode narrative metadata of chunk %s: %w", row.ID, err)
		}
		out[row.ID] = verifyChunk{Content: row.Content, DocumentName: row.DocumentName, Meta: meta}
	}
	return out, nil
}

// citationChunkIDs 收集引用涉及的去重后的 chunk id，顺序确定（宪法第 V 条）。
func citationChunkIDs(cites []RelationCitation) []string {
	seen := make(map[string]bool, len(cites))
	out := make([]string, 0, len(cites))
	for _, c := range cites {
		if seen[c.ChunkID] {
			continue
		}
		seen[c.ChunkID] = true
		out = append(out, c.ChunkID)
	}
	return out
}

// verifyCitationsAgainstChunks 逐条比对，返回**仍然对得上**的那些。
//
// ⭐ 比对的是"这段文档区间现在映射到的文字"与"当初记下的 quote"，
// 不是"quote 在块里能不能找到"。⚠️ 后者是错的：一句在书里出现多次的话，
// 无论区间偏成什么样都能"找到"，核验形同虚设。
//
// 顺带把 DocumentName 补上——它只有 PG 侧有，而对话层渲染引用要用。
func verifyCitationsAgainstChunks(cites []RelationCitation, chunks map[string]verifyChunk) ([]RelationCitation, int) {
	kept := make([]RelationCitation, 0, len(cites))
	dropped := 0
	for _, c := range cites {
		ch, ok := chunks[c.ChunkID]
		if !ok || ch.Meta == nil {
			// 块已经不在当前已发布版本里了。
			dropped++
			continue
		}
		proj, err := newChunkProjection(ch.Content, *ch.Meta)
		if err != nil {
			dropped++
			continue
		}
		ts, te, ok := proj.fromDocument(c.SourceStart, c.SourceEnd)
		if !ok {
			dropped++
			continue
		}
		text := []rune(proj.Text)
		if ts < 0 || te > len(text) || string(text[ts:te]) != c.Quote {
			dropped++
			continue
		}
		c.DocumentName = ch.DocumentName
		kept = append(kept, c)
	}
	return kept, dropped
}

// errRelationScopeChanged 表示这一轮读到的记录已经被并发变化取代。
//
// ⚠️ 这不是"查不到"，也不是可以降级处理的错误：文档被删掉、改版，
// 或者用户重新发起了一次抽取之后，手上这批引用属于一个**已经不存在的
// 上下文**。继续拿它们去回答，用户看到的是上一轮的结论配着这一轮的进度。
var errRelationScopeChanged = errors.New("knowledge: relation scope changed during query")

// recheckRelationScope 是入模前贴着模型调用的那次 MySQL 复检。
func (r *Repository) recheckRelationScope(ctx context.Context, docID string, version int64, jobID string) error {
	doc, err := r.queries.GetDocumentExtractionState(ctx, docID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 文档在这次查询期间被删掉了。
			return errRelationScopeChanged
		}
		return fmt.Errorf("knowledge: recheck document for relation query: %w", err)
	}
	if doc.Version != version {
		return errRelationScopeChanged
	}
	if !doc.IsRelationExtractionEnabled {
		return errRelationScopeChanged
	}
	if !doc.ActiveRelationJobID.Valid || doc.ActiveRelationJobID.String != jobID {
		// 新的一轮抽取已经接管了这份文档。
		return errRelationScopeChanged
	}
	return nil
}
