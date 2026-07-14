-- name: CreateMCPServer :exec
INSERT INTO mcp_servers (
    id, name, transport, command, args, env, url, headers, created_by
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetMCPServerByID :one
SELECT id, name, transport, command, args, env, url, headers, status,
       last_synced_at, last_error, is_active, created_by, created_at, updated_at
FROM mcp_servers
WHERE id = ?;

-- name: ListMCPServers :many
SELECT id, name, transport, command, args, env, url, headers, status,
       last_synced_at, last_error, is_active, created_by, created_at, updated_at
FROM mcp_servers
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountMCPServers :one
SELECT COUNT(*) FROM mcp_servers;

-- name: UpdateMCPServer :exec
UPDATE mcp_servers
SET name = ?, command = ?, args = ?, env = ?, url = ?, headers = ?, is_active = ?
WHERE id = ?;

-- name: UpdateMCPServerSyncResult :exec
UPDATE mcp_servers
SET status = ?, last_synced_at = ?, last_error = ?
WHERE id = ?;
