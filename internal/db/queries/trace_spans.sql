-- name: CreateTraceSpan :exec
INSERT INTO trace_spans (
    id, trace_id, parent_span_id, conversation_id, kind, name, status,
    input, output, error_message, attrs, started_at, finished_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListTraceSpansByConversation :many
SELECT id, trace_id, parent_span_id, conversation_id, kind, name, status,
       input, output, error_message, attrs, started_at, finished_at
FROM trace_spans
WHERE conversation_id = ?
ORDER BY started_at ASC;
