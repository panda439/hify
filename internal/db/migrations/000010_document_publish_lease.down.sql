-- 回滚假设没有行处于 publishing 状态（正常运维流程：先把 publishing 文档
-- 处理完/清空再降级）——如果还有 publishing 行，恢复旧 CHECK 约束这一步会
-- 报错，这和这个仓库其它迁移的 down 脚本一样不做额外的数据迁移安全网。
ALTER TABLE documents
    DROP INDEX idx_documents_status_lease,
    DROP COLUMN lease_expires_at,
    DROP CHECK chk_documents_status,
    ADD CONSTRAINT chk_documents_status
        CHECK (status IN ('pending', 'processing', 'ready', 'failed'));
