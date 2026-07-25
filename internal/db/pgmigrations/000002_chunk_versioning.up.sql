-- document_version 对应 MySQL documents.version：一批 chunks 属于文档的
-- 哪一次处理尝试。is_published 是可见性开关——SearchChunks/
-- CountChunksByKnowledgeBase 只看 is_published=true 的行，未发布的草稿版本
-- 物理写入了也检索不到。
--
-- 重跑（含首次处理）的落库顺序：新版本 chunks 先以 is_published=false 写入
-- -> MySQL CAS 确认这次尝试仍是权威版本 -> 同一 PG 事务内把旧版本删掉、新
-- 版本标记为已发布。MySQL 的 CAS 是唯一仲裁点：只有它确认赢家之后，PG 侧
-- 才允许把结果变得可搜索，这样一个被判定卡死、被 reconciliation 取代的
-- 旧 worker 永远没有机会把过时结果发布出去。
--
-- 存量行（迁移前已处理完成的文档）默认 document_version=1、
-- is_published=true——按"已发布的 version 1"对待，迁移后立即可搜索，不
-- 需要重新处理。
ALTER TABLE chunks ADD COLUMN document_version bigint NOT NULL DEFAULT 1;
ALTER TABLE chunks ADD COLUMN is_published boolean NOT NULL DEFAULT true;

CREATE INDEX idx_chunks_document_id_version ON chunks (document_id, document_version);
