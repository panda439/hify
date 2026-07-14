-- name: CreateAgentMCPTool :exec
INSERT INTO agent_mcp_tools (agent_id, mcp_tool_id) VALUES (?, ?);

-- name: DeleteAgentMCPTools :exec
DELETE FROM agent_mcp_tools WHERE agent_id = ?;

-- name: ListMCPToolIDsByAgent :many
SELECT mcp_tool_id FROM agent_mcp_tools WHERE agent_id = ?;

-- name: ListAgentIDsByMCPTool :many
SELECT agent_id FROM agent_mcp_tools WHERE mcp_tool_id = ?;
