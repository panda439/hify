-- name: CreateKnowledgeBase :exec
INSERT INTO knowledge_bases (
    id, name, description, embedding_model_id, chunk_size, chunk_overlap, created_by
) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetKnowledgeBaseByID :one
SELECT id, name, description, embedding_model_id, chunk_size, chunk_overlap,
       is_active, created_by, created_at, updated_at
FROM knowledge_bases
WHERE id = ?;

-- name: ListKnowledgeBases :many
SELECT id, name, description, embedding_model_id, chunk_size, chunk_overlap,
       is_active, created_by, created_at, updated_at
FROM knowledge_bases
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountKnowledgeBases :one
SELECT COUNT(*) FROM knowledge_bases;

-- name: UpdateKnowledgeBase :exec
-- embedding_model_id/chunk_size/chunk_overlap are deliberately not
-- updatable here — see the "创建后不可修改" note in the plan's
-- knowledge_bases design.
UPDATE knowledge_bases
SET name = ?, description = ?, is_active = ?
WHERE id = ?;
