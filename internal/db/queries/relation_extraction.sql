-- 010-narrative-scene-chunking-and-relation-extraction：抽取作业的编排与账目。
--
-- ⚠️ 本文件里没有任何一条 SELECT * 或隐式列清单：这些表大半是账目，
-- 加一列而某条查询没跟上，表现是"某个数字少算了一部分"，不报错。

-- name: CreateRelationExtractionJob :exec
-- 建 job。source_hash 为 NULL 表示尚未枚举语料——不是"空文档"。
INSERT INTO relation_extraction_jobs (
    id, document_id, knowledge_base_id, document_version, run_number,
    model_id, config_hash, config_snapshot,
    approved_item_limit, call_limit, active_ms_limit,
    operation_key_hash, operation_request_hash, state
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'initializing');

-- name: GetRelationExtractionJob :one
-- ⚠️ **故意不选三个 JSON 列**（config_snapshot / archived_ledger_summary /
-- budget_operations）。两个理由：
--  1. 这是热路径——租约心跳每 30 秒就要用它做一次"我还是不是当前作业"的
--     检查，没必要每次都把配置快照和账目归档整块拉回来；
--  2. sqlc 把可空 JSON 映射成 json.RawMessage，而它扫不了 NULL
--     （unsupported Scan, storing driver.Value type <nil>），
--     那两列在作业刚建好时**正常就是 NULL**。
-- 需要它们的报表/预算路径走 GetRelationExtractionJobPayload。
SELECT id, document_id, knowledge_base_id, document_version, run_number,
       model_id, config_hash, source_hash,
       state, stop_reason, epoch, lease_until, heartbeat_at, initialization_complete,
       total_items, succeeded_items, failed_items,
       approved_item_limit, call_limit, active_ms_limit, retry_rounds,
       reserved_calls, confirmed_dispatches, unknown_attempts,
       active_ms_used, active_ms_reserved,
       started_at, finished_at,
       operation_key_hash, operation_request_hash,
       created_at, updated_at
FROM relation_extraction_jobs WHERE id = ?;

-- name: GetRelationExtractionJobPayload :one
-- 三个 JSON 列单独取。⚠️ 可空的两列在 Go 侧用 sql.NullString 承接
-- （见上面的注释），由 repository 转成领域类型时再解析。
SELECT config_snapshot,
       CAST(archived_ledger_summary AS CHAR) AS archived_ledger_summary,
       CAST(budget_operations AS CHAR) AS budget_operations
FROM relation_extraction_jobs WHERE id = ?;

-- name: GetRelationExtractionJobByOperationKey :one
-- 幂等键重放：同一个 start/restart 请求打第二次，返回已有的 run 而不是新开。
SELECT id, document_id, document_version, run_number, state,
       operation_key_hash, operation_request_hash
FROM relation_extraction_jobs
WHERE document_id = ? AND operation_key_hash = ?;

-- name: CreateRelationExtractionItem :exec
INSERT INTO relation_extraction_items (id, job_id, chunk_id, chunk_index, content_hash)
VALUES (?, ?, ?, ?, ?);

-- name: CountRelationExtractionItems :one
SELECT COUNT(*) FROM relation_extraction_items WHERE job_id = ?;

-- name: CompleteJobInitialization :execrows
-- ⭐ 初始化完成是一次**带守卫的**状态跃迁，不是无条件 UPDATE。
--
-- 守卫 state='initializing' AND initialization_complete=0：
-- 两个 worker 同时初始化同一个 job 时，只有一个能跃迁成功，另一个拿到 0 行
-- 并放弃自己的整个事务。没有这个守卫，第二个会把 total_items 覆盖成自己数出来
-- 的值——而它枚举的可能是另一个版本的 chunk，数字看起来完全正常。
UPDATE relation_extraction_jobs
SET state = 'running', initialization_complete = 1,
    total_items = ?, source_hash = ?, started_at = ?, updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND state = 'initializing' AND initialization_complete = 0;

-- name: FailRelationExtractionJob :execrows
-- ⚠️ 只在 job 还没结束时生效。已经 succeeded/failed 的 job 不该被一条迟到的
-- 失败改写——那条失败属于一个早就被取代的 epoch。
UPDATE relation_extraction_jobs
SET state = 'failed', stop_reason = ?, finished_at = ?, lease_until = NULL,
    updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND state IN ('initializing', 'running', 'paused');

-- name: ClaimRelationExtractionJob :execrows
-- ⭐ 抢占：epoch + 1，写租约。守卫里的 lease_until 条件是「没人持有，或者
-- 持有者的租约已经过期」。
--
-- ⚠️ epoch 自增是**唯一**能区分"我还是当前持有者"的东西。worker 之后每次
-- 写数据都要带上自己抢到的 epoch；租约过期后被别人抢走，旧 worker 迟到的写入
-- 会因为 epoch 对不上被拒。⚠️ 它**只约束数据发布**——旧 worker 那次外部调用
-- 该花的钱已经花了，账目照记，见 relation_extraction_attempts。
UPDATE relation_extraction_jobs
SET epoch = epoch + 1, lease_until = ?, heartbeat_at = ?,
    updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND state IN ('initializing', 'running')
  AND (lease_until IS NULL OR lease_until < ?);

-- name: RenewRelationExtractionLease :execrows
-- 心跳续租，必须带 epoch：租约已经被别人抢走时返回 0 行，
-- 持有者据此知道自己已经出局，必须停止调用模型。
UPDATE relation_extraction_jobs
SET lease_until = ?, heartbeat_at = ?, updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND epoch = ? AND state IN ('initializing', 'running');

-- name: ReleaseRelationExtractionLease :execrows
UPDATE relation_extraction_jobs
SET lease_until = NULL, updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND epoch = ?;

-- name: SetDocumentRelationJob :execrows
-- 把文档的 active_relation_job_id 指向新 run，并落下所选模型。
-- ⚠️ 守卫 status='ready' AND version=?：文档在这中间改了版本，
-- 这次开启就该失败，而不是把作业挂到一批已经不是真相的 chunk 上。
UPDATE documents
SET active_relation_job_id = ?, relation_model_id = ?, updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND status = 'ready' AND version = ?;

-- name: GetDocumentExtractionState :one
SELECT id, status, version, is_narrative, is_relation_extraction_enabled,
       relation_model_id, active_relation_job_id
FROM documents WHERE id = ?;

-- ---------------------------------------------------------------------
-- attempt 账目：这张表是"成本数字可信"的全部依据
-- ---------------------------------------------------------------------

-- name: ReserveExtractionAttempt :exec
-- ⭐ **先记 reserved，再发外部调用**。顺序不可颠倒。
--
-- 颠倒的后果：进程在"已发出、未收到"之间崩掉，这次调用不会留下任何痕迹，
-- 而它的钱已经花了。恢复扫描把停留过久的 reserved 改判 unknown，
-- 于是"可能花了"这件事被如实记下来——这正是 unknown 这个状态存在的理由。
INSERT INTO relation_extraction_attempts (
    id, job_id, item_id, epoch, phase, attempt_number,
    request_hash, max_output_tokens, state
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'reserved');

-- name: SettleExtractionAttempt :execrows
-- 结算一次尝试。⚠️ 守卫 state='reserved'：一次尝试只能被结算一次，
-- 重复结算会让 usage 和费用被重复累加进上层聚合。
UPDATE relation_extraction_attempts
SET state = ?, started_at = ?, finished_at = ?, elapsed_ms = ?,
    dispatch_confirmed = ?, usage_known = ?, input_tokens = ?, output_tokens = ?,
    finish_reason = ?, error_code = ?,
    raw_response = ?, response_hash = ?,
    cost_amount = ?, currency = ?, pricing_version = ?, cost_kind = ?
WHERE id = ? AND state = 'reserved';

-- name: ClassifyJobBudgetState :one
-- 预留被拒之后**再问一次**是哪一维用尽了。
--
-- ⚠️ 两条语句之间理论上还能再变（另一个 worker 又花了一点），但用途只是
-- 给用户一句准确的话（"调用额度用尽"还是"活跃时间用尽"），
-- 而两者的下一步不同：前者追加调用额度，后者说明模型变慢了、追加时间
-- 未必解决问题。把它做成一条语句的代价是每次预留都多算两个布尔值。
SELECT reserved_calls >= call_limit AS calls_exhausted,
       active_ms_used >= active_ms_limit AS active_time_exhausted
FROM relation_extraction_jobs WHERE id = ?;

-- name: AddJobCallReservation :execrows
-- 预留一次调用额度。⚠️ 守卫 reserved_calls < call_limit：预算耗尽时返回 0 行，
-- 调用方据此停手。把预算检查放在**同一条 UPDATE 的 WHERE 里**而不是先读后写，
-- 是因为后者在两个 worker 之间必然超发——而超发的表现是账单超了，不报错。
UPDATE relation_extraction_jobs
SET reserved_calls = reserved_calls + 1, updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND epoch = ? AND reserved_calls < call_limit
  AND active_ms_used < active_ms_limit;

-- name: RefundJobCallReservation :execrows
-- 退还一次**确定没发出去**的调用预留（限流/熔断/拿不到并发槽）。
-- ⚠️ 只有 not_dispatched 能走这里。unknown 绝不退——那笔钱可能已经花了。
UPDATE relation_extraction_jobs
SET reserved_calls = reserved_calls - 1, updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND reserved_calls > 0;

-- name: RecordJobDispatchOutcome :execrows
-- 结算后把结局计进作业级账目。confirmed_dispatches 与 unknown_attempts
-- 分开计：前者是"确定发生过"，后者是"可能发生过"，报告里必须分别呈现，
-- 合并会让一个不确定的数字看起来像确定的。
UPDATE relation_extraction_jobs
SET confirmed_dispatches = confirmed_dispatches + ?,
    unknown_attempts = unknown_attempts + ?,
    active_ms_used = active_ms_used + ?,
    updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ?;

-- name: GetExtractionAttempt :one
SELECT id, job_id, item_id, epoch, phase, attempt_number, request_hash,
       max_output_tokens, state, created_at, started_at, finished_at, elapsed_ms,
       dispatch_confirmed, usage_known, input_tokens, output_tokens,
       finish_reason, error_code, response_hash,
       cost_amount, currency, pricing_version, cost_kind
FROM relation_extraction_attempts WHERE id = ?;

-- name: GetExtractionAttemptRawResponse :one
-- 原始响应单独取：它最大 64 KiB，不该出现在任何列表或统计查询里。
SELECT raw_response FROM relation_extraction_attempts WHERE id = ?;

-- name: ListStaleReservedAttempts :many
-- 恢复扫描：停留在 reserved 太久的尝试。⚠️ 它们**不是**没发生过——
-- 进程在收到响应之前崩了，所以要改判 unknown 而不是删掉或标 failed。
SELECT id, job_id, item_id, created_at
FROM relation_extraction_attempts
WHERE state = 'reserved' AND created_at < ?
ORDER BY created_at, id
LIMIT ?;

-- name: MarkAttemptUnknown :execrows
UPDATE relation_extraction_attempts
SET state = 'unknown', error_code = ?, finished_at = ?
WHERE id = ? AND state = 'reserved';

-- name: CountJobAttemptsByState :many
SELECT state, COUNT(*) AS n FROM relation_extraction_attempts
WHERE job_id = ? GROUP BY state ORDER BY state;

-- name: BumpItemAttemptCount :execrows
-- ⚠️ 尝试次数落在**数据库**里，不在内存。放内存的表现是：worker 崩溃重启后
-- 计数归零，于是一个永远会失败的 item 被无限重试下去，把预算烧光——
-- 而每一轮看起来都正常。这是自动恢复最容易引入的一种死循环。
UPDATE relation_extraction_items
SET extract_attempt_count = extract_attempt_count + ?,
    alias_attempt_count = alias_attempt_count + ?,
    state = 'running', updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ?;

-- name: GetItemAttemptCounts :one
SELECT extract_attempt_count, alias_attempt_count, state
FROM relation_extraction_items WHERE id = ?;

-- ---------------------------------------------------------------------
-- 成功结果的发布：人物 / 关系 / 证据 / item 状态 / 计数，同一个事务
-- ---------------------------------------------------------------------

-- name: CreateNarrativeCharacter :exec
INSERT INTO narrative_characters
    (id, job_id, display_name, first_source_order, identity_evidence, has_ambiguity)
VALUES (?, ?, ?, ?, ?, ?);

-- name: UpsertNarrativeRelation :exec
-- ⚠️ INSERT IGNORE 而不是普通 INSERT：同一条关系可能因为回放（响应已落盘、
-- 发布前崩溃）被再写一次。唯一键 (job_id, relation_key_hash) 让第二次成为
-- 无操作，而不是让整个回放失败。
--
-- ⚠️ 这里的"重复"只指**同一处出处的同一条关系**。跨章、同章不同场景的
-- 同类型关系 key 不同，会各自成行——关系历史不按当前状态覆盖，
-- 那是这个功能的立论。
INSERT IGNORE INTO narrative_relations
    (id, job_id, subject_id, object_id, relation_type, is_directed,
     relation_key_hash, first_source_order, chapter_number, chapter_title)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetNarrativeRelationByKey :one
SELECT id FROM narrative_relations WHERE job_id = ? AND relation_key_hash = ?;

-- name: UpsertNarrativeRelationEvidence :exec
-- ⚠️ 唯一键是 (relation_id, evidence_key_hash)，而 evidence_key 由
-- **规范源区间 + quote hash** 算出，**不含 chunk_id**：相邻 chunk 因 overlap
-- 会包含同一段原文，按 chunk_id 去重会把同一处出处记成两条证据，
-- 虚增后面要写进报告的证据条数。
INSERT IGNORE INTO narrative_relation_evidence
    (id, job_id, relation_id, chunk_id, document_version, source_order,
     source_start, source_end, quote, source_segments, evidence_key_hash)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: MarkItemSucceeded :execrows
-- ⚠️ 守卫 state <> 'succeeded'：一个 item 只能成功一次，否则
-- succeeded_items 会被重复累加，而它是覆盖率的分子。
UPDATE relation_extraction_items
SET state = 'succeeded', extract_response = ?, alias_response = ?,
    last_error_code = NULL, updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND job_id = ? AND state <> 'succeeded';

-- name: MarkItemFailed :execrows
UPDATE relation_extraction_items
SET state = 'failed', last_error_code = ?, updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND job_id = ? AND state NOT IN ('succeeded', 'failed');

-- name: BumpJobItemOutcome :execrows
-- ⚠️ 守卫 epoch：过期 worker 的迟到发布不得改动计数。
UPDATE relation_extraction_jobs
SET succeeded_items = succeeded_items + ?, failed_items = failed_items + ?,
    updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND epoch = ?;

-- name: FindReplayableAttempt :one
-- 回放：这个 item 的这个阶段是否已经有一次**成功且原始响应已落盘**的尝试。
--
-- ⭐ 有的话，恢复的 worker 必须拿它接着算，**不能再打一次模型**。
-- 再打一次的后果不是"结果不一致"，是那笔钱白花第二遍，而账目上看起来
-- 完全正常——两次都是真实发生的调用。
SELECT id, raw_response, response_hash, finish_reason
FROM relation_extraction_attempts
WHERE item_id = ? AND phase = ? AND state = 'completed' AND raw_response IS NOT NULL
ORDER BY attempt_number DESC
LIMIT 1;

-- name: ListRecoverableExtractionJobs :many
-- 恢复扫描：需要有人接手的作业。
--
-- ⭐ state 白名单里**故意没有** paused 和 budget_exhausted：
-- 那两个是**用户或预算做出的决定**，不是故障。自动把它们捡回来跑，
-- 等于系统擅自推翻了一次显式的停止——而用户会看到一个自己明明暂停过的
-- 作业又开始花钱。
--
-- 条件是"没人持有，或者持有者的租约已经过期"。id 收尾做游标分页，
-- 避免一次扫描把成千上万行拉回来。
SELECT id, document_id, document_version, epoch, state, initialization_complete
FROM relation_extraction_jobs
WHERE state IN ('initializing', 'running')
  AND (lease_until IS NULL OR lease_until < ?)
  AND id > ?
ORDER BY id
LIMIT ?;

-- name: LockDocumentForExtraction :one
-- ⭐ 锁顺序的第一环：document → job → item。
--
-- ⚠️ 顺序不一致的表现是**偶发死锁**：两个 worker 各持一半的锁互相等，
-- MySQL 超时后杀掉其中一个。它只在并发操作同一份文档时出现，
-- 单元测试跑一百次可能一次都不复现，而生产上会周期性地丢掉一个 item
-- 并留下一条难以归因的错误。所以每个涉及多张表的事务都从这里开始。
SELECT id, status, version, is_narrative, is_relation_extraction_enabled
FROM documents WHERE id = ? FOR UPDATE;

-- name: SupersedePriorExtractionJobs :execrows
-- restart：把这份文档上此前的作业全部标为 superseded。
--
-- ⚠️ 只把文档指针改到新作业是不够的：旧作业的 state 还是 running，
-- 恢复扫描会把它当成"崩溃的作业"捡回来接着跑——于是两个 run 同时对同一份
-- 文档花钱，而两者看起来都健康。
--
-- ⚠️ paused / budget_exhausted 也一并取代。它们不该被**自动**恢复
-- （见 ListRecoverableExtractionJobs），但用户显式 restart 就是在替换它们；
-- 留着不动会让文档上挂着两个都不是 superseded 的历史作业，
-- 账目查询分不清哪一个是当前 run。
UPDATE relation_extraction_jobs
SET state = 'superseded', finished_at = ?, lease_until = NULL,
    updated_at = CURRENT_TIMESTAMP(3)
WHERE document_id = ? AND id <> ?
  AND state IN ('initializing', 'running', 'paused', 'budget_exhausted');

-- ---------------------------------------------------------------------
-- 清理与账目归档
-- ---------------------------------------------------------------------

-- name: ListJobsWithArchivableAttempts :many
-- 哪些作业有过期的 attempt 可以归档。
--
-- ⚠️ 分批的单位是**作业**，不是行。一个作业的 attempt 上限就是它的 call_limit
-- （默认 3000），一次事务处理这么多行是可以接受的；而按行分批会让"求和"和
-- "删除"必须对齐同一批行，多出一整套游标对齐的复杂度，换来的只是更小的事务。
SELECT DISTINCT job_id FROM relation_extraction_attempts
WHERE finished_at IS NOT NULL AND finished_at < ?
ORDER BY job_id
LIMIT ?;

-- name: SumArchivableAttempts :one
-- 归档前先把这一批的账目求和。
--
-- ⭐ token 只在 usage_known 时计入，并单独统计"有多少次调用是知道用量的"。
-- ⚠️ 把未知当 0 相加，就是把"没测到"和"真的没花"混成一个数——而那正是
-- 000017 的 CHECK 和整条账目链路一路在防的事。报告里必须能说出
-- "token 数只覆盖 N/M 次调用"。
SELECT COUNT(*) AS attempts,
       COALESCE(SUM(dispatch_confirmed), 0) AS confirmed_dispatches,
       COALESCE(SUM(state = 'unknown'), 0) AS unknown_attempts,
       COALESCE(SUM(usage_known), 0) AS usage_known_attempts,
       COALESCE(SUM(CASE WHEN usage_known THEN input_tokens ELSE 0 END), 0) AS input_tokens,
       COALESCE(SUM(CASE WHEN usage_known THEN output_tokens ELSE 0 END), 0) AS output_tokens,
       COALESCE(SUM(elapsed_ms), 0) AS active_ms,
       COALESCE(SUM(cost_amount), 0) AS cost_amount
FROM relation_extraction_attempts
WHERE job_id = ? AND finished_at IS NOT NULL AND finished_at < ?;

-- name: DeleteArchivableAttempts :execrows
-- ⚠️ 谓词必须与 SumArchivableAttempts **逐字相同**。不同的话，求和覆盖的
-- 行和删掉的行就不是同一批：多删的那些费用永远消失，少删的那些下一轮会被
-- 再加一遍。两种偏差都不报错。
DELETE FROM relation_extraction_attempts
WHERE job_id = ? AND finished_at IS NOT NULL AND finished_at < ?;

-- name: UpdateJobArchivedSummary :execrows
UPDATE relation_extraction_jobs
SET archived_ledger_summary = ?, updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ?;

-- name: SumLiveAttempts :one
-- 未归档 attempt 的账目。⭐ 查询费用 = 本查询 + archived_ledger_summary。
--
-- ⚠️ **不能**用 jobs 表上的 confirmed_dispatches / active_ms_used 再加归档汇总：
-- 那两列是**全生命周期**计数，归档时并不减少，加上汇总就是把同一批调用
-- 算了两遍。两条口径必须择一，这里择"活账 + 归档汇总"，因为它在
-- 清理之后仍然成立。
SELECT COUNT(*) AS attempts,
       COALESCE(SUM(dispatch_confirmed), 0) AS confirmed_dispatches,
       COALESCE(SUM(state = 'unknown'), 0) AS unknown_attempts,
       COALESCE(SUM(usage_known), 0) AS usage_known_attempts,
       COALESCE(SUM(CASE WHEN usage_known THEN input_tokens ELSE 0 END), 0) AS input_tokens,
       COALESCE(SUM(CASE WHEN usage_known THEN output_tokens ELSE 0 END), 0) AS output_tokens,
       COALESCE(SUM(elapsed_ms), 0) AS active_ms,
       COALESCE(SUM(cost_amount), 0) AS cost_amount
FROM relation_extraction_attempts WHERE job_id = ?;

-- name: ListDeadJobsWithDerivedRows :many
-- 被取代 / 失败的作业，其派生记录已经不可查询，可以清理。
--
-- ⚠️ 只清 superseded / failed。succeeded 的**不清**：那是用户当前能查到的
-- 关系数据。paused / budget_exhausted 也不清——它们随时可能被继续。
SELECT id FROM relation_extraction_jobs
WHERE state IN ('superseded', 'failed') AND finished_at IS NOT NULL AND finished_at < ?
ORDER BY id
LIMIT ?;

-- name: DeleteJobEvidence :execrows
DELETE FROM narrative_relation_evidence WHERE job_id = ?;

-- name: DeleteJobRelations :execrows
DELETE FROM narrative_relations WHERE job_id = ?;

-- name: DeleteJobAliases :execrows
DELETE FROM narrative_aliases WHERE job_id = ?;

-- name: DeleteJobCharacters :execrows
DELETE FROM narrative_characters WHERE job_id = ?;
