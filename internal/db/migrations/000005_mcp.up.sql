CREATE TABLE mcp_servers (
    id CHAR(36) NOT NULL,
    name VARCHAR(255) NOT NULL,
    transport VARCHAR(16) NOT NULL,
    command VARCHAR(500) NULL,
    args JSON NULL,
    env JSON NULL,
    url VARCHAR(500) NULL,
    headers JSON NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'unknown',
    last_synced_at DATETIME(3) NULL,
    last_error TEXT NULL,
    is_active TINYINT(1) NOT NULL DEFAULT 1,
    created_by CHAR(36) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_mcp_servers_created_by (created_by),
    CONSTRAINT chk_mcp_servers_transport CHECK (transport IN ('stdio', 'sse')),
    CONSTRAINT chk_mcp_servers_status CHECK (status IN ('unknown', 'connected', 'failed'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE mcp_tools (
    id CHAR(36) NOT NULL,
    mcp_server_id CHAR(36) NOT NULL,
    tool_name VARCHAR(255) NOT NULL,
    description TEXT NOT NULL,
    input_schema JSON NOT NULL,
    is_active TINYINT(1) NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_mcp_tools_server_name (mcp_server_id, tool_name),
    KEY idx_mcp_tools_mcp_server_id (mcp_server_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE agent_mcp_tools (
    agent_id CHAR(36) NOT NULL,
    mcp_tool_id CHAR(36) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (agent_id, mcp_tool_id),
    KEY idx_agent_mcp_tools_tool_id (mcp_tool_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;
