CREATE EXTENSION IF NOT EXISTS vector;

-- chunks 从 MySQL 整表迁到 PostgreSQL（内容+向量放一起）：检索是单库单条
-- SQL，没有内容双写一致性问题；chunks 是可再生的派生数据（documents +
-- 磁盘原文件仍在 MySQL/本地盘），丢了重新处理文档即可恢复。
--
-- CLAUDE.md 的 MySQL 建表约定翻译到 PG：
--   id：仍是应用层生成的 UUIDv7 字符串，但用 text 不用 char(36)/uuid——
--       PG 的 char(36) 有补空格语义，text 才是惯用等价物；用原生 uuid 类型
--       会迫使 sqlc 对 uuid 和 uuid[]（ANY 参数）做额外 override，不值得。
--       UUIDv7 前缀是时间戳，text btree 里仍保有插入局部性。
--   created_at：timestamptz(3) ~ DATETIME(3)+UTC（容器固定 timezone=UTC）。
--   ENGINE/CHARSET/ROW_FORMAT 是 MySQL 专属子句，PG 无对应物。
--
-- embedding 故意不声明维度（vector 而非 vector(N)）：不同知识库可配置不同
-- embedding 模型、维度不一，同一张表要装得下所有维度；查询恒带
-- embedding_dimension 过滤。代价是无维度列不能建 HNSW/IVFFlat 索引——当前
-- 每知识库 5000 chunks 软上限下，pgvector 精确顺序扫描是毫秒级，且已远快于
-- 旧实现（全量拉回 Go 逐条打分），不值得为此付 ANN 的近似召回代价。
-- 若未来规模突破需要 ANN，出路是按实际在用的维度建部分表达式索引：
--   CREATE INDEX idx_chunks_hnsw_1536 ON chunks
--     USING hnsw ((embedding::vector(1536)) vector_cosine_ops)
--     WHERE embedding_dimension = 1536;
-- 并在 repository 手写一条把维度字面量内联进同样 cast 表达式的查询——
-- planner 只匹配字面一致的表达式，sqlc 的静态 SQL 表达不了这个。
CREATE TABLE chunks (
    id text NOT NULL,
    knowledge_base_id text NOT NULL,
    document_id text NOT NULL,
    chunk_index integer NOT NULL,
    content text NOT NULL,
    content_length integer NOT NULL,
    embedding vector NOT NULL,
    embedding_dimension integer NOT NULL,
    created_at timestamptz(3) NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);

CREATE INDEX idx_chunks_knowledge_base_id ON chunks (knowledge_base_id);
CREATE INDEX idx_chunks_document_id ON chunks (document_id);
