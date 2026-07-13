-- name: CreateMessage :exec
INSERT INTO messages (
    id, conversation_id, role, content, tool_calls, tool_call_id, token_count
) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListRecentMessagesByConversation :many
-- Most recent N messages, newest first. Used both for context assembly
-- (caller re-orders chronologically and truncates by token budget) and for
-- the first page of a conversation's history in the UI — always bounded by
-- conversation_id per CLAUDE.md's large-table rule (never an unfiltered scan).
SELECT id, conversation_id, role, content, tool_calls, tool_call_id, token_count, created_at
FROM messages
WHERE conversation_id = ?
ORDER BY created_at DESC, id DESC
LIMIT ?;

-- name: ListMessagesByConversationBeforeCursor :many
-- Keyset page for "load more" history, strictly older than the given
-- (created_at, id) cursor — same index as ListRecentMessagesByConversation.
-- Written as an OR-expansion rather than a (created_at, id) < (?, ?) row
-- constructor: sqlc's mysql parser doesn't reliably detect placeholders
-- inside a tuple comparison (it silently generated a 2-arg function for a
-- 4-placeholder query when tried), so this is the safe form.
SELECT id, conversation_id, role, content, tool_calls, tool_call_id, token_count, created_at
FROM messages
WHERE conversation_id = ?
  AND (created_at < ? OR (created_at = ? AND id < ?))
ORDER BY created_at DESC, id DESC
LIMIT ?;
