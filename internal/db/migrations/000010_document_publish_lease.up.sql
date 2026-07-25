-- 新增 publishing 中间态：processing -> publishing -> ready，只有 PG 发布
-- （幂等的 DeleteObsoleteChunkVersions + PublishChunkVersion）真正成功后才
-- CAS 到 ready。这样 PG 发布失败或 worker 在发布成功后、CAS ready 之前崩溃
-- 这两个窗口都不会再让 MySQL 永久显示 ready 而 PG 侧毫无进展——文档留在
-- publishing，由 reconciliation 用租约到期判定重新执行同一段幂等发布逻辑。
--
-- lease_expires_at 把 processing/publishing 阶段"是否卡死"的判定，从覆盖
-- 整份文档的固定阈值（旧的 staleProcessingThreshold，单文档最多 ~63 次
-- Embed 调用很容易超过任何固定阈值）换成心跳续租：worker 每完成一批
-- Embedding、每次状态转换都续一次租，reconciliation 只回收租约真正过期的
-- 文档——见 knowledge/model.go 的 leaseDuration 注释。
ALTER TABLE documents
    DROP CHECK chk_documents_status,
    ADD CONSTRAINT chk_documents_status
        CHECK (status IN ('pending', 'processing', 'publishing', 'ready', 'failed')),
    ADD COLUMN lease_expires_at DATETIME(3) NULL AFTER version,
    ADD INDEX idx_documents_status_lease (status, lease_expires_at);
