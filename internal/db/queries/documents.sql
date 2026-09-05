-- name: CreateDocument :exec
INSERT INTO documents (
    id, knowledge_base_id, file_name, file_type, file_size, storage_path, created_by
) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetDocumentByID :one
SELECT id, knowledge_base_id, file_name, file_type, file_size, storage_path,
       status, error_message, chunk_count, created_by, created_at, updated_at,
       version, lease_expires_at, unextracted_pages
FROM documents
WHERE id = ?;

-- name: ListDocumentsByKnowledgeBase :many
SELECT id, knowledge_base_id, file_name, file_type, file_size, storage_path,
       status, error_message, chunk_count, created_by, created_at, updated_at,
       version, lease_expires_at, unextracted_pages
FROM documents
WHERE knowledge_base_id = ?
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountDocumentsByKnowledgeBase :one
SELECT COUNT(*) FROM documents WHERE knowledge_base_id = ?;

-- name: DeleteDocument :exec
DELETE FROM documents WHERE id = ?;

-- 以下都是文档处理状态机的 CAS 转换——见 knowledge/service.go 的
-- ProcessDocument。每条都带 id+version+旧状态三重限定，0 行受影响是预期
-- 内的常见结果（并发重复到达、任务已过期、租约续约被抢），不是错误。
--
-- 状态机：pending/failed -> processing -> publishing -> ready，失败分支
-- processing -> failed。lease_expires_at 是 processing/publishing 两个
-- "worker 正持有这个文档"阶段的心跳租约，claim/续租/转 publishing 都会
-- 刷新它，转到 ready/failed 会清空——见 knowledge/model.go 的 leaseDuration
-- 注释。

-- name: ClaimDocumentProcessing :execrows
-- pending/failed -> processing，同时种下初始租约。两次并发到达同一
-- (id,version) 时，只有一次能把 0 行变成 1 行——这是需求"重复执行不产生
-- 重复 chunks"的第一道防线。
UPDATE documents
SET status = 'processing', lease_expires_at = sqlc.arg(lease_expires_at)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND status IN ('pending', 'failed');

-- name: RenewDocumentLease :execrows
-- worker 每完成一批 Embedding、每个关键阶段前都调它续租；status 作为参数
-- 传入，processing/publishing 两个阶段复用同一条 SQL。0 行受影响 = 这个
-- worker 已经被 reconciliation 判定卡死并取代（version 或 status 已经不
-- 匹配），调用方必须立刻停手，不能再写 chunks 或发布。
UPDATE documents
SET lease_expires_at = sqlc.arg(lease_expires_at)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND status = sqlc.arg(status);

-- name: MarkDocumentPublishing :execrows
-- processing -> publishing：Embedding 已经全部完成、chunks 已经以
-- is_published=false 写入 PG 之后才能到这一步——这一步"锁定"了"活儿已经
-- 干完，只差发布"，reconciliation 之后即使要恢复也不需要重新跑一遍
-- Embedding，只需要重跑幂等的 PG 发布（见 MarkDocumentReady）。
UPDATE documents
SET status = 'publishing', lease_expires_at = sqlc.arg(lease_expires_at)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND status = 'processing';

-- name: MarkDocumentReady :execrows
-- publishing -> ready，是发布完成后的最终确认：只有 PG 发布（幂等的
-- DeleteObsoleteChunkVersions + PublishChunkVersion）真正成功后才允许调用
-- 这一步。0 行受影响说明这次尝试已经被别的 runner（原 worker 自己，或者
-- reconciliation 的恢复流程）抢先完成了——不是错误，是良性的幂等竞争。
--
-- ⭐ 007-document-processing-notice 把 unextracted_pages 的写入并进了这一条，
-- 而不是单开一条语句。这里是"一个版本成为 ready"的**唯一**入口，而且它已经
-- 带着本功能需要的全部保证：
--   * WHERE version = ? AND status = 'publishing' 保证写进去的提示属于**最终
--     生效的那一次**处理，而不是被淘汰的那次——本功能因此一行并发代码都不用写；
--   * 它已经在无条件清 error_message，把 unextracted_pages 加在旁边语义完全对称：
--     **每次成功整体覆盖，没有缺页就写 NULL**。「重新处理后不再缺页则提示消失」
--     由此是**免费得到的**，不需要任何额外语句。
--
-- ⚠️ **禁止**为清除提示单开一条语句。两条语句之间没有事务，就有一个窗口，
-- 窗口里文档的状态和提示是不一致的——而这种不一致没有任何报错，只会让用户
-- 看到一条过期的提示。
UPDATE documents
SET status = 'ready', error_message = NULL, chunk_count = ?,
    unextracted_pages = ?, lease_expires_at = NULL
WHERE id = ? AND version = ? AND status = 'publishing';

-- name: MarkDocumentFailed :execrows
-- processing -> failed。发布阶段（publishing）的失败不走这条路——按设计
-- 文档留在 publishing，交给 reconciliation 用幂等发布恢复，不转 failed。
UPDATE documents
SET status = 'failed', error_message = ?, chunk_count = 0, lease_expires_at = NULL
WHERE id = ? AND version = ? AND status = 'processing';

-- name: ReclaimDocumentForRetry :execrows
-- 人工重试 API 用：pending/failed -> pending 且 version 前进一位，让旧
-- version 的任何延迟到达的任务实例在后续 CAS 里天然被判定过期。pending/
-- failed 从不持有租约，不需要touch lease_expires_at。
UPDATE documents
SET status = 'pending', version = sqlc.arg(new_version)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(old_version) AND status IN ('pending', 'failed');

-- name: ReclaimStaleProcessingDocument :execrows
-- reconciliation 用：认领一个 processing 租约已过期（worker 大概率已经
-- 崩溃/丢失）的文档，version 前进一位、清空租约后重新排队，从头开始处理
-- （Embedding 还没做完，没有可以复用的中间产物）。
--
-- lease_expires_at 的两个条件（非空 + 早于 expired_before）不是可省的
-- 冗余校验：id/version/status 只能证明"这一行看起来还是扫描时的那一行"，
-- 证明不了"扫描之后没人续过租"——一个活跃 worker 完全可能在 reconciliation
-- 查完、UPDATE 执行前的窗口里成功续租，此时 id/version/status 全都没变，
-- 缺了这两个条件 UPDATE 照样会命中，把一个正常运行的任务错误回收（经典
-- check-then-act/TOCTOU）。expired_before 必须是调用方本轮扫描时定下的
-- 同一个时间快照（ReconcileStuckDocuments 的 now），不是这里现算——否则
-- "扫描看到过期"和"CAS 校验过期"就是两个不同时刻的判断，窗口依然存在。
-- 多个 Hify 实例并发对同一行执行这条 UPDATE 时，MySQL 的行锁保证只有先
-- 提交的一个能让 lease_expires_at 实际改变，后一个的 WHERE 条件在它执行
-- 时已经不成立，天然 0 行受影响，不需要额外的分布式锁。
UPDATE documents
SET status = 'pending', version = sqlc.arg(new_version), lease_expires_at = NULL
WHERE id = sqlc.arg(id) AND version = sqlc.arg(old_version) AND status = 'processing'
  AND lease_expires_at IS NOT NULL AND lease_expires_at < sqlc.arg(expired_before);

-- name: ClaimExpiredPublishingRecovery :execrows
-- reconciliation 用：在对一个卡在 publishing 的文档调用 publishAndComplete
-- 之前，先通过这条 CAS 取得"恢复权"——同样是为了堵住 TOCTOU 窗口：只看
-- id/version/status='publishing' 无法排除"reconciliation 查询之后、真正
-- 发起恢复动作之前，原 worker 自己续租成功并继续往下走"的情况。PG 发布
-- 本身是幂等的，但幂等只保证"重复执行不产生脏数据"，不等于"不需要恢复
-- 权"——没有这条 CAS，两个并发的恢复者（reconciliation 和活跃 worker，
-- 或者两个 Hify 实例的 reconciliation）会一起对同一个 version 跑发布/
-- CAS ready，虽然结果无害但完全是偶然，不是设计保证的。
-- CAS 成功后把租约刷新成一份新的"恢复租约"（new_lease_expires_at，调用方
-- 传 newLeaseDeadline()）：如果这次恢复尝试的 PG 发布又失败了，文档留在
-- publishing、带着这份新租约，等它自己过期后才轮到下一轮 reconciliation
-- 再抢，不会被同一轮或紧接着的下一轮重复认领。
UPDATE documents
SET lease_expires_at = sqlc.arg(new_lease_expires_at)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND status = 'publishing'
  AND lease_expires_at IS NOT NULL AND lease_expires_at < sqlc.arg(expired_before);

-- name: ListLeaseExpiredProcessingDocuments :many
-- reconciliation 扫描用：processing 状态且租约已过期，大概率是 worker
-- 崩溃留下的孤儿任务。LIMIT 防止单次 reconciliation 运行处理量失控。
SELECT id, knowledge_base_id, file_name, file_type, file_size, storage_path,
       status, error_message, chunk_count, created_by, created_at, updated_at,
       version, lease_expires_at, unextracted_pages
FROM documents
WHERE status = 'processing' AND lease_expires_at IS NOT NULL AND lease_expires_at < ?
LIMIT 100;

-- name: ListLeaseExpiredPublishingDocuments :many
-- reconciliation 扫描用：publishing 状态且租约已过期——Embedding 已经做
-- 完，只是发布这一步没能走到底（PG 发布失败，或者 worker 在发布成功后、
-- CAS ready 之前崩溃）。
SELECT id, knowledge_base_id, file_name, file_type, file_size, storage_path,
       status, error_message, chunk_count, created_by, created_at, updated_at,
       version, lease_expires_at, unextracted_pages
FROM documents
WHERE status = 'publishing' AND lease_expires_at IS NOT NULL AND lease_expires_at < ?
LIMIT 100;

-- name: ListStalePendingDocuments :many
-- reconciliation 扫描用：pending 状态停留超过阈值，大概率是入队失败（见
-- UploadDocument 的注释）导致没有任何任务在处理它。pending 从没有 worker
-- 持有过租约，"入队丢了"这个问题只能靠 updated_at 阈值判断。
SELECT id, knowledge_base_id, file_name, file_type, file_size, storage_path,
       status, error_message, chunk_count, created_by, created_at, updated_at,
       version, lease_expires_at, unextracted_pages
FROM documents
WHERE status = 'pending' AND updated_at < ?
LIMIT 100;
