-- message_citations 持久化 Citation V1 的引用快照——message_id/ref 是主键
-- （见 CLAUDE.md 建表约定的复合主键写法），knowledge_base_id/document_id/
-- document_name/chunk_id/chunk_index/quote/score 全部是"这一轮真正交给模型
-- 的证据"在保存那一刻的快照，不建跨库外键（document_id/chunk_id 指向
-- PostgreSQL 的 documents/chunks，MySQL 无法对它们做引用完整性校验），文档
-- 或分片以后被删除/重新处理都不影响这里已经保存的历史记录。
--
-- message_id 是唯一的真外键：citations 依附于它所属的 assistant message
-- 存在，message 删除时级联删除，不留孤儿行。
--
-- score 用 DECIMAL(5,4) 不用 FLOAT——CLAUDE.md 小数字段规范；pgvector 余弦
-- 相似度落在 [-1, 1]，4 位小数精度和 SearchChunks 返回的 float8 展示精度
-- 一致，5 位总长度覆盖 "-1.0000" 到 "1.0000"。
CREATE TABLE message_citations (
    message_id CHAR(36) NOT NULL,
    ref VARCHAR(8) NOT NULL,
    knowledge_base_id CHAR(36) NOT NULL,
    document_id CHAR(36) NOT NULL,
    document_name VARCHAR(255) NOT NULL,
    chunk_id CHAR(36) NOT NULL,
    chunk_index INT NOT NULL,
    quote TEXT NOT NULL,
    score DECIMAL(5,4) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (message_id, ref),
    CONSTRAINT fk_message_citations_message
        FOREIGN KEY (message_id) REFERENCES messages (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;
