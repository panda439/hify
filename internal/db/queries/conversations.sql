-- name: CreateConversation :exec
INSERT INTO conversations (id, agent_id, user_id, title) VALUES (?, ?, ?, ?);

-- name: GetConversationByID :one
SELECT id, agent_id, user_id, title, created_at, updated_at
FROM conversations
WHERE id = ?;

-- name: ListConversationsByUser :many
SELECT id, agent_id, user_id, title, created_at, updated_at
FROM conversations
WHERE user_id = ?
ORDER BY updated_at DESC
LIMIT ? OFFSET ?;

-- name: CountConversationsByUser :one
SELECT COUNT(*) FROM conversations WHERE user_id = ?;

-- name: TouchConversation :exec
UPDATE conversations SET updated_at = ? WHERE id = ?;
