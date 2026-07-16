-- name: CreateChunk :exec
INSERT INTO chunks (
    id, knowledge_base_id, document_id, chunk_index, content,
    content_length, embedding, embedding_dimension
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: SearchChunks :many
-- <=> 是 pgvector 的余弦「距离」（0=同向 2=反向），1 - 距离 = 余弦相似度，
-- 和被删掉的 similarity.go 语义完全一致——分数跨迁移可比。
-- 两个 WHERE 条件都不可省：knowledge_base_id 是 CLAUDE.md 大表查询强过滤
-- 规则；embedding_dimension 过滤是因为混合维度共存一张表，<=> 对不同维度
-- 向量直接报错。::text[] cast 也不可省——sqlc 生成的代码用 pq.Array 传字符
-- 串数组，显式 cast 消除 PG 的类型推断歧义。
SELECT id, knowledge_base_id, document_id, chunk_index, content,
       content_length, embedding_dimension, created_at,
       (1 - (embedding <=> sqlc.arg(query_embedding)))::float8 AS score
FROM chunks
WHERE knowledge_base_id = ANY(sqlc.arg(knowledge_base_ids)::text[])
  AND embedding_dimension = sqlc.arg(embedding_dimension)
ORDER BY embedding <=> sqlc.arg(query_embedding)
LIMIT sqlc.arg(top_k);

-- name: CountChunksByKnowledgeBase :one
SELECT COUNT(*) FROM chunks WHERE knowledge_base_id = $1;

-- name: DeleteChunksByDocument :exec
DELETE FROM chunks WHERE document_id = $1;
