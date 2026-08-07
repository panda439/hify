-- name: CreateChunk :exec
-- 新版本 chunks 一律以 is_published=false 写入——"新版本写入"这一步不改变
-- 任何已发布的旧版本可见性，发布是 PublishChunkVersion 单独一步。
-- document_name 是处理时刻的 Document.FileName 快照（Citation 用的来源
-- 展示名，见 pgmigrations 000003）；page_number/section_title 当前解析器
-- 不产出可靠值，调用方一律传 NULL，不允许伪造。
INSERT INTO chunks (
    id, knowledge_base_id, document_id, chunk_index, content,
    content_length, embedding, embedding_dimension, document_version, is_published,
    document_name, page_number, section_title
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: SearchVectorChunks :many
-- Phase 3 前叫 SearchChunks——引入 SearchKeywordChunks 之后改名以便和它
-- 对称、消歧义，语义完全不变。<=> 是 pgvector 的余弦「距离」（0=同向
-- 2=反向），1 - 距离 = 余弦相似度，和被删掉的 similarity.go 语义完全
-- 一致——分数跨迁移可比。
-- 三个 WHERE 条件都不可省：knowledge_base_id 是 CLAUDE.md 大表查询强过滤
-- 规则；embedding_dimension 过滤是因为混合维度共存一张表，<=> 对不同维度
-- 向量直接报错；is_published 过滤是版本可见性网关——未发布的草稿版本永远
-- 不能被检索命中，即使已经物理写入。::text[] cast 也不可省——sqlc 生成的
-- 代码用 pq.Array 传字符串数组，显式 cast 消除 PG 的类型推断歧义。
-- document_name/page_number/section_title 是 Citation V1 需要的来源
-- metadata，随 chunk 一起返回给 conversation 层，knowledge 自己不解释它们。
-- top_k 由调用方传入——Hybrid Search 场景下调用方传的是 candidateK（比
-- 最终 topK 更宽的候选窗口，给 RRF 融合留排序空间），不是最终 topK 本身。
-- ORDER BY 第二个键 id ASC 是稳定兜底：<=> 距离相同时 PostgreSQL 不保证
-- 返回顺序，而 rrfFuse（hybrid.go）把这个结果切片的位置索引当成 RRF 的
-- rank——距离相同却顺序不定，会让同一批候选在两次调用之间拿到不同的
-- rank，进而拿到不同的 fusionScore，最终排序不稳定。id 是主键，任何两行
-- 都不会相等，兜底到底。
SELECT id, knowledge_base_id, document_id, chunk_index, content,
       content_length, embedding_dimension, created_at,
       document_name, page_number, section_title,
       (1 - (embedding <=> sqlc.arg(query_embedding)))::float8 AS score
FROM chunks
WHERE knowledge_base_id = ANY(sqlc.arg(knowledge_base_ids)::text[])
  AND embedding_dimension = sqlc.arg(embedding_dimension)
  AND is_published = true
ORDER BY embedding <=> sqlc.arg(query_embedding), id ASC
LIMIT sqlc.arg(top_k);

-- name: SearchKeywordChunks :many
-- pg_trgm 字符级 trigram/word-similarity 关键词检索（lexical search）——
-- 明确不是 BM25：BM25 需要真正的分词 + 词频/逆文档频率统计，pg_trgm 只是
-- 字符 n-gram 相似度，中英文都能用但不做语言学分词，也不做真正的相关性
-- 排序模型。见 pgmigrations 000004 的索引和阈值说明。
--
-- 不依赖 embedding，因此没有 embedding_dimension 过滤——这正是关键词检索
-- 在 embedding 服务失败/超时时仍能独立返回结果的原因（见 service.go 的
-- Retrieve：向量一路失败只跳过对应 embedding model 分组，关键词一路继续）。
-- knowledge_base_id / is_published 两个过滤的必要性和 SearchVectorChunks
-- 相同：大表强过滤规则、未发布草稿版本永不可检索。
--
-- query_text <> '' 是防御性的第二道闸——Go 层 Retrieve/searchKeywordChunks
-- 已经在空 query 时直接短路不落库查询，这里再挡一层，防止将来有调用方
-- 绕开 Go 层校验时，<% 空串在语义上退化成"什么都不过滤"从而让全表都
-- 变成候选。
-- <% 走 idx_chunks_content_trgm 这个 GIN 索引过滤候选集（判定阈值见
-- pgmigrations 000004 里对 pg_trgm.word_similarity_threshold 的说明）；
-- ORDER BY 里的 word_similarity() 只对索引已经筛出的候选重新计算一次精确
-- 分数用于排序，而不是对全表算。candidate_k 是硬上限，防止候选集无界增长。
SELECT id, knowledge_base_id, document_id, chunk_index, content,
       content_length, embedding_dimension, created_at,
       document_name, page_number, section_title,
       word_similarity(sqlc.arg(query_text), content)::float8 AS score
FROM chunks
WHERE knowledge_base_id = ANY(sqlc.arg(knowledge_base_ids)::text[])
  AND is_published = true
  AND sqlc.arg(query_text) <> ''
  AND sqlc.arg(query_text) <% content
ORDER BY word_similarity(sqlc.arg(query_text), content) DESC, id ASC
LIMIT sqlc.arg(candidate_k);

-- name: CountChunksByKnowledgeBase :one
-- 只数已发布的——未发布的草稿版本不该出现在"这个知识库有多少分片"的统计里。
SELECT COUNT(*) FROM chunks WHERE knowledge_base_id = $1 AND is_published = true;

-- name: DeleteChunksByDocument :exec
-- 整份文档删除用（DeleteDocument）：不分版本、不看发布状态，全部清空。
DELETE FROM chunks WHERE document_id = $1;

-- name: DeleteChunksByDocumentVersion :exec
-- 精确删除某一个 version 的 chunks——CAS 发布网关被拒绝（version 已过期）
-- 时，worker 用它清理自己刚写入、永远不会被发布的那批草稿行。
DELETE FROM chunks WHERE document_id = $1 AND document_version = $2;

-- name: DeleteObsoleteChunkVersions :exec
-- 发布步骤的"清理旧版本"半部分：删掉除了刚发布的这个 version 之外的所有
-- 行。和下面 PublishChunkVersion 一起在同一个 PG 事务里调用（见
-- repository.go 的 publishDocumentVersion），保证 PG 内部不会出现"新旧
-- 同时可见"或者"新旧都不可见"的中间态。
DELETE FROM chunks WHERE document_id = $1 AND document_version <> $2;

-- name: PublishChunkVersion :exec
-- 发布步骤的"发布新版本"半部分。
UPDATE chunks SET is_published = true WHERE document_id = $1 AND document_version = $2;

-- name: CountChunksByDocumentVersion :one
-- reconciliation 恢复卡在 publishing 的文档时用它现查这次尝试实际发布了
-- 多少行，写回 MySQL 的 chunk_count——reconciliation 和原 worker 不是同一
-- 次调用，没有 worker 当时算出来的 len(pieces) 可用。
SELECT COUNT(*) FROM chunks WHERE document_id = $1 AND document_version = $2 AND is_published = true;
