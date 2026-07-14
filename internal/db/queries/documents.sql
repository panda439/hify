-- name: CreateDocument :exec
INSERT INTO documents (
    id, knowledge_base_id, file_name, file_type, file_size, storage_path, created_by
) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetDocumentByID :one
SELECT id, knowledge_base_id, file_name, file_type, file_size, storage_path,
       status, error_message, chunk_count, created_by, created_at, updated_at
FROM documents
WHERE id = ?;

-- name: ListDocumentsByKnowledgeBase :many
SELECT id, knowledge_base_id, file_name, file_type, file_size, storage_path,
       status, error_message, chunk_count, created_by, created_at, updated_at
FROM documents
WHERE knowledge_base_id = ?
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountDocumentsByKnowledgeBase :one
SELECT COUNT(*) FROM documents WHERE knowledge_base_id = ?;

-- name: UpdateDocumentStatus :exec
-- Written by the asynq worker as it moves a document through
-- pending -> processing -> ready|failed.
UPDATE documents
SET status = ?, error_message = ?, chunk_count = ?
WHERE id = ?;

-- name: DeleteDocument :exec
DELETE FROM documents WHERE id = ?;
