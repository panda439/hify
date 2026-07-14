-- name: UpsertMCPTool :exec
-- Sync is idempotent: re-running it against the same server just refreshes
-- description/input_schema for tools that still exist and reactivates a
-- tool that had previously disappeared and come back.
INSERT INTO mcp_tools (id, mcp_server_id, tool_name, description, input_schema, is_active)
VALUES (?, ?, ?, ?, ?, 1)
ON DUPLICATE KEY UPDATE
    description = VALUES(description),
    input_schema = VALUES(input_schema),
    is_active = 1;

-- name: ListMCPToolsByServer :many
SELECT id, mcp_server_id, tool_name, description, input_schema, is_active, created_at, updated_at
FROM mcp_tools
WHERE mcp_server_id = ?
ORDER BY tool_name;

-- name: GetMCPToolByID :one
SELECT id, mcp_server_id, tool_name, description, input_schema, is_active, created_at, updated_at
FROM mcp_tools
WHERE id = ?;

-- name: UpdateMCPToolActive :exec
UPDATE mcp_tools SET is_active = ? WHERE id = ?;
