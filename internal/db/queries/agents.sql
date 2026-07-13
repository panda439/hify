-- name: CreateAgent :exec
INSERT INTO agents (
    id, name, description, model_id, system_prompt, temperature,
    max_tokens, top_p, extra_params, created_by
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetAgentByID :one
SELECT id, name, description, model_id, system_prompt, temperature, max_tokens,
       top_p, extra_params, is_active, created_by, created_at, updated_at
FROM agents
WHERE id = ?;

-- name: ListAgents :many
SELECT id, name, description, model_id, system_prompt, temperature, max_tokens,
       top_p, extra_params, is_active, created_by, created_at, updated_at
FROM agents
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountAgents :one
SELECT COUNT(*) FROM agents;

-- name: UpdateAgent :exec
UPDATE agents
SET name = ?, description = ?, model_id = ?, system_prompt = ?, temperature = ?,
    max_tokens = ?, top_p = ?, extra_params = ?, is_active = ?
WHERE id = ?;
