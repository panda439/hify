CREATE TABLE knowledge_bases (
    id CHAR(36) NOT NULL,
    name VARCHAR(255) NOT NULL,
    description VARCHAR(1000) NOT NULL DEFAULT '',
    embedding_model_id CHAR(36) NOT NULL,
    chunk_size INT NOT NULL DEFAULT 500,
    chunk_overlap INT NOT NULL DEFAULT 50,
    is_active TINYINT(1) NOT NULL DEFAULT 1,
    created_by CHAR(36) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_knowledge_bases_embedding_model_id (embedding_model_id),
    KEY idx_knowledge_bases_created_by (created_by)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE documents (
    id CHAR(36) NOT NULL,
    knowledge_base_id CHAR(36) NOT NULL,
    file_name VARCHAR(255) NOT NULL,
    file_type VARCHAR(16) NOT NULL,
    file_size INT NOT NULL,
    storage_path VARCHAR(500) NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'pending',
    error_message TEXT NULL,
    chunk_count INT NOT NULL DEFAULT 0,
    created_by CHAR(36) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_documents_kb_status (knowledge_base_id, status),
    CONSTRAINT chk_documents_file_type CHECK (file_type IN ('txt', 'md', 'pdf')),
    CONSTRAINT chk_documents_status CHECK (status IN ('pending', 'processing', 'ready', 'failed'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE chunks (
    id CHAR(36) NOT NULL,
    knowledge_base_id CHAR(36) NOT NULL,
    document_id CHAR(36) NOT NULL,
    chunk_index INT NOT NULL,
    content TEXT NOT NULL,
    content_length INT NOT NULL,
    embedding JSON NOT NULL,
    embedding_dimension INT NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_chunks_knowledge_base_id (knowledge_base_id),
    KEY idx_chunks_document_id (document_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE agent_knowledge_bases (
    agent_id CHAR(36) NOT NULL,
    knowledge_base_id CHAR(36) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (agent_id, knowledge_base_id),
    KEY idx_agent_knowledge_bases_kb_id (knowledge_base_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;
