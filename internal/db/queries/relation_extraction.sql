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

-- name: AddJobCallReservation :execrows
-- 预留一次调用额度。⚠️ 守卫 reserved_calls < call_limit：预算耗尽时返回 0 行，
-- 调用方据此停手。把预算检查放在**同一条 UPDATE 的 WHERE 里**而不是先读后写，
-- 是因为后者在两个 worker 之间必然超发——而超发的表现是账单超了，不报错。
UPDATE relation_extraction_jobs
SET reserved_calls = reserved_calls + 1, updated_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND epoch = ? AND reserved_calls < call_limit;

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
