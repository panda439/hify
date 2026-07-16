-- 按 000004_knowledge.up.sql 的原 DDL 重建。IF NOT EXISTS 是给 sqlc 的：
-- sqlc 按文件名字典序解析 schema 目录下所有 .sql（包括 down 文件），本文件
-- 排在 000004 up（已建表）之后、000007 up（drop）之前，不加 IF NOT EXISTS
-- 会在 make sqlc 时报 "table already exists"。
CREATE TABLE IF NOT EXISTS chunks (
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
