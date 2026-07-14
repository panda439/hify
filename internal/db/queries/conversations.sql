-- name: CreateConversation :exec
INSERT INTO conversations (id, agent_id, user_id, title) VALUES (?, ?, ?, ?);

-- name: GetConversationByID :one
SELECT id, agent_id, user_id, title, created_at, updated_at
FROM conversations
WHERE id = ?;

-- name: ListConversationsByUser :many
-- last_message backs the sidebar preview snippet — a correlated subquery
-- against messages is fine here because conversations is a small,
-- offset-paginated table (per CLAUDE.md's pagination rules) and each
-- lookup hits idx_messages_conversation_created with LIMIT 1. COALESCE to
-- '' matters: a conversation with no messages yet (the gap between
-- creating it and sending the first message) would otherwise make this
-- subquery return SQL NULL, and sqlc generates a plain non-nullable string
-- field for it — scanning a real NULL into that panics at runtime.
SELECT c.id, c.agent_id, c.user_id, c.title, c.created_at, c.updated_at,
       COALESCE((SELECT m.content FROM messages m
        WHERE m.conversation_id = c.id
        ORDER BY m.created_at DESC, m.id DESC LIMIT 1), '') AS last_message
FROM conversations c
WHERE c.user_id = ?
ORDER BY c.updated_at DESC
LIMIT ? OFFSET ?;

-- name: CountConversationsByUser :one
SELECT COUNT(*) FROM conversations WHERE user_id = ?;

-- name: TouchConversation :exec
UPDATE conversations SET updated_at = ? WHERE id = ?;
