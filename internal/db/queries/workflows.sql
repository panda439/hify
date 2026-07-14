-- name: CreateWorkflow :exec
INSERT INTO workflows (
    id, name, description, definition, created_by
) VALUES (?, ?, ?, ?, ?);

-- name: GetWorkflowByID :one
SELECT id, name, description, definition, is_active, created_by, created_at, updated_at
FROM workflows
WHERE id = ?;

-- name: ListWorkflows :many
SELECT id, name, description, definition, is_active, created_by, created_at, updated_at
FROM workflows
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountWorkflows :one
SELECT COUNT(*) FROM workflows;

-- name: UpdateWorkflow :exec
UPDATE workflows
SET name = ?, description = ?, definition = ?, is_active = ?
WHERE id = ?;
