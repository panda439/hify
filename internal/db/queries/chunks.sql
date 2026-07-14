-- name: CreateChunk :exec
INSERT INTO chunks (
    id, knowledge_base_id, document_id, chunk_index, content,
    content_length, embedding, embedding_dimension
) VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListChunksByKnowledgeBase :many
-- Used to (re)build the per-knowledge-base embedding-matrix cache — always
-- bounded by knowledge_base_id per CLAUDE.md's large-table rule, no LIMIT
-- since the caller wants the full set (soft-capped at upload time, not
-- read time).
SELECT id, knowledge_base_id, document_id, chunk_index, content,
       content_length, embedding, embedding_dimension, created_at
FROM chunks
WHERE knowledge_base_id = ?
ORDER BY document_id, chunk_index;

-- name: CountChunksByKnowledgeBase :one
SELECT COUNT(*) FROM chunks WHERE knowledge_base_id = ?;

-- name: DeleteChunksByDocument :exec
DELETE FROM chunks WHERE document_id = ?;
