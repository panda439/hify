-- 010-narrative-scene-chunking-and-relation-extraction：叙事分块开关与关系抽取账目。
--
-- 本期的核心产出是**两个真实数字**：全书规模的抽取成本，以及三元组的
-- 准确率/召回率。下面这套表里超过一半的列不是为了「让功能跑起来」，
-- 而是为了**让这两个数字算得出来且可复核**——每一次外部调用、它的
-- 原始响应、它花了多少、它是否真的发出去过，都要留痕。少记一样，
-- 报告里就只能写一个无法回溯的数字，那和没有数字差别不大。

-- ---------------------------------------------------------------------
-- documents 增列
-- ---------------------------------------------------------------------
--
-- ⚠️ is_narrative **上传时固定，之后不可切换**。存量文档一律 0，行为不变。
-- 允许切换等于允许「同一份文档的不同片段用不同分块方式产生」，那样
-- chunk_index 的含义在一份文档内部就不再一致，而这件事没有任何报错。
--
-- ⚠️ 抽取开关与叙事开关**分开两列**，不合并成一个三值字段：叙事分块是
-- 确定性纯函数，抽取要调模型、花钱、可能失败。把它们绑成一个开关，
-- 用户就无法「只要场景分块、不要抽取」，而那恰恰是默认想要的组合。
ALTER TABLE documents
    ADD COLUMN is_narrative TINYINT(1) NOT NULL DEFAULT 0,
    ADD COLUMN is_relation_extraction_enabled TINYINT(1) NOT NULL DEFAULT 0,
    ADD COLUMN relation_model_id CHAR(36) NULL,
    ADD COLUMN active_relation_job_id CHAR(36) NULL;

ALTER TABLE documents
    ADD KEY idx_documents_relation_model_id (relation_model_id),
    ADD KEY idx_documents_active_relation_job_id (active_relation_job_id),
    -- 恢复扫描用：找「已就绪且开了抽取」的文档，id 收尾做游标分页。
    ADD KEY idx_documents_relation_recovery (status, is_relation_extraction_enabled, id);

-- ⭐ 只有叙事文档能开抽取。这条能写成 CHECK 是因为它只依赖同一行的两列，
-- 不需要跨表。写在数据库里而不是只写在 Service 里，是因为「非叙事文档
-- 开了抽取」这种行会让初始化阶段拿不到 scene_key，然后**静默产出一批
-- 没有场景归属的关系记录**——没有报错，只有一堆无法定位的三元组。
ALTER TABLE documents
    ADD CONSTRAINT chk_documents_relation_requires_narrative
        CHECK (is_relation_extraction_enabled = 0 OR is_narrative = 1);

-- ---------------------------------------------------------------------
-- relation_extraction_jobs：一次抽取运行的全部事实与账目
-- ---------------------------------------------------------------------
CREATE TABLE relation_extraction_jobs (
    id CHAR(36) NOT NULL,
    document_id CHAR(36) NOT NULL,
    knowledge_base_id CHAR(36) NOT NULL,
    -- 复用 documents.version，不另造同义的 content_version：两个含义相同
    -- 的版本列迟早会不一致，而不一致的表现是「作业挂在旧版本的 chunk 上」，
    -- 同样不报错。
    document_version INT NOT NULL,
    run_number INT NOT NULL,

    model_id CHAR(36) NOT NULL,
    config_hash BINARY(32) NOT NULL,
    config_snapshot JSON NOT NULL,
    -- 初始化之前语料 hash 未知；NULL 表示「还没算」，不是「空文档」。
    source_hash BINARY(32) NULL,

    state VARCHAR(32) NOT NULL DEFAULT 'pending',
    stop_reason VARCHAR(64) NULL,
    -- epoch 是租约代数：worker 每次抢到作业 +1，写数据时带上自己的 epoch，
    -- 过期 worker 的迟到写入被拒。它**只约束数据发布**，不代表外部推理
    -- 没有重复发生——那笔钱已经花了，账要照记。
    epoch INT NOT NULL DEFAULT 0,
    lease_until DATETIME(3) NULL,
    heartbeat_at DATETIME(3) NULL,
    initialization_complete TINYINT(1) NOT NULL DEFAULT 0,

    total_items INT NOT NULL DEFAULT 0,
    succeeded_items INT NOT NULL DEFAULT 0,
    failed_items INT NOT NULL DEFAULT 0,

    approved_item_limit INT NOT NULL,
    call_limit INT NOT NULL,
    active_ms_limit BIGINT NOT NULL,
    retry_rounds INT NOT NULL DEFAULT 0,

    -- ⭐ 预留与确认分开记。先 reserved 再发外部调用，崩溃时那笔预留会变成
    -- unknown 而不是消失——**不能声称没发出去**。只记确认数的账目会在
    -- 每次崩溃后系统性地低估成本，而且是往好看的方向偏。
    reserved_calls INT NOT NULL DEFAULT 0,
    confirmed_dispatches INT NOT NULL DEFAULT 0,
    unknown_attempts INT NOT NULL DEFAULT 0,
    active_ms_used BIGINT NOT NULL DEFAULT 0,
    active_ms_reserved BIGINT NOT NULL DEFAULT 0,

    started_at DATETIME(3) NULL,
    finished_at DATETIME(3) NULL,
    -- 过期 attempt 删除前，按批把计数累加到这里。查询费用 =
    -- 未归档 attempt + 本汇总，两者**不重叠**；既算全生命周期计数又加汇总
    -- 会把成本翻倍。
    archived_ledger_summary JSON NULL,

    operation_key_hash BINARY(32) NULL,
    operation_request_hash BINARY(32) NULL,
    budget_operations JSON NULL,

    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),

    PRIMARY KEY (id),
    UNIQUE KEY uk_rej_document_version_run (document_id, document_version, run_number),
    -- 幂等键：同一个 start/restart 请求重放不会开出第二个 run。
    UNIQUE KEY uk_rej_document_operation (document_id, operation_key_hash),
    KEY idx_rej_knowledge_base_id (knowledge_base_id),
    KEY idx_rej_model_id (model_id),
    KEY idx_rej_state_lease (state, lease_until, id),
    KEY idx_rej_document_state (document_id, state, id),
    CONSTRAINT chk_rej_state CHECK (state IN
        ('pending','initializing','running','paused','succeeded','failed','superseded','budget_exhausted')),
    CONSTRAINT chk_rej_counts_nonneg CHECK (
        total_items >= 0 AND succeeded_items >= 0 AND failed_items >= 0
        AND reserved_calls >= 0 AND confirmed_dispatches >= 0 AND unknown_attempts >= 0
        AND active_ms_used >= 0 AND active_ms_reserved >= 0 AND retry_rounds >= 0),
    CONSTRAINT chk_rej_limits_positive CHECK (
        approved_item_limit > 0 AND call_limit > 0 AND active_ms_limit > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

-- ---------------------------------------------------------------------
-- relation_extraction_items：一个 chunk 一行
-- ---------------------------------------------------------------------
CREATE TABLE relation_extraction_items (
    id CHAR(36) NOT NULL,
    job_id CHAR(36) NOT NULL,
    -- ⚠️ chunk 住在 PostgreSQL，这里是**逻辑引用**，跨库建不了外键。
    -- 因此发布和读取两侧都要各自做一次实际的 parent/version 检查，
    -- 不能靠数据库替你挡住悬空引用。
    chunk_id CHAR(36) NOT NULL,
    chunk_index INT NOT NULL,
    content_hash BINARY(32) NOT NULL,

    state VARCHAR(32) NOT NULL DEFAULT 'pending',
    extract_attempt_count INT NOT NULL DEFAULT 0,
    alias_attempt_count INT NOT NULL DEFAULT 0,
    -- ⚠️ 这里存的是**通过校验的最终结果**。校验不过的原始输出留在 attempt
    -- 表里，不搬到这里——搬过来就等于让一次失败冒充成功。
    extract_response JSON NULL,
    alias_response JSON NULL,
    last_error_code VARCHAR(64) NULL,

    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),

    PRIMARY KEY (id),
    UNIQUE KEY uk_rei_job_chunk (job_id, chunk_id),
    UNIQUE KEY uk_rei_job_index (job_id, chunk_index),
    KEY idx_rei_chunk_id (chunk_id),
    KEY idx_rei_job_state (job_id, state, chunk_index, id),
    CONSTRAINT chk_rei_state CHECK (state IN ('pending','running','succeeded','failed')),
    CONSTRAINT chk_rei_attempts_nonneg CHECK (
        extract_attempt_count >= 0 AND alias_attempt_count >= 0 AND chunk_index >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

-- ---------------------------------------------------------------------
-- relation_extraction_attempts：每一次外部调用的凭据
-- ---------------------------------------------------------------------
--
-- ⭐ 这张表是「成本数字可信」的**全部依据**。没有它，报告里的成本就是
-- 一个无法复核的总数。
CREATE TABLE relation_extraction_attempts (
    id CHAR(36) NOT NULL,
    job_id CHAR(36) NOT NULL,
    item_id CHAR(36) NOT NULL,
    epoch INT NOT NULL,
    phase VARCHAR(16) NOT NULL,
    attempt_number INT NOT NULL,

    request_hash BINARY(32) NOT NULL,
    max_output_tokens INT NOT NULL,

    state VARCHAR(16) NOT NULL DEFAULT 'reserved',
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    started_at DATETIME(3) NULL,
    finished_at DATETIME(3) NULL,
    elapsed_ms BIGINT NULL,

    -- dispatch_confirmed=0 且 state='unknown' 就是「可能发出去了也可能没有」。
    -- 这一格必须能被表达；把它塞进 failed 会低估成本，塞进 completed 会
    -- 凭空造出一条不存在的响应。
    dispatch_confirmed TINYINT(1) NOT NULL DEFAULT 0,
    -- ⚠️ usage_known=0 时 input/output_tokens 为 NULL，**不是 0**。
    -- 不是所有兼容供应商都返回 token 用量（见 internal/CLAUDE.md），
    -- 用 0 兜底会让"没测到"和"真的没花"变得无法区分。
    usage_known TINYINT(1) NOT NULL DEFAULT 0,
    input_tokens INT NULL,
    output_tokens INT NULL,
    finish_reason VARCHAR(64) NULL,
    error_code VARCHAR(64) NULL,

    raw_response LONGTEXT NULL,
    response_hash BINARY(32) NULL,

    cost_amount DECIMAL(18,8) NULL,
    currency VARCHAR(8) NULL,
    pricing_version VARCHAR(32) NULL,
    -- 本地 Ollama 没有金钱计费：cost_kind='not_applicable'、amount=NULL。
    -- ⚠️ 不写 0——0 的意思是"花了零元"，而真实情况是"这个口径不适用"，
    -- 硬件电力成本没有测量。报告里必须照这个区分写。
    cost_kind VARCHAR(16) NOT NULL DEFAULT 'unknown',

    PRIMARY KEY (id),
    UNIQUE KEY uk_rea_item_phase_attempt (item_id, phase, attempt_number),
    KEY idx_rea_job_created (job_id, created_at, id),
    KEY idx_rea_item_id (item_id),
    KEY idx_rea_state_created (state, created_at, id),
    CONSTRAINT chk_rea_phase CHECK (phase IN ('extract','alias')),
    CONSTRAINT chk_rea_state CHECK (state IN
        ('reserved','dispatched','completed','failed','unknown','not_dispatched')),
    CONSTRAINT chk_rea_cost_kind CHECK (cost_kind IN
        ('measured','estimated','unknown','not_applicable')),
    -- ⭐ 两条「不许拿 0 冒充未知」的不变量，写死在数据库里。
    CONSTRAINT chk_rea_usage_consistent CHECK (
        usage_known = 1 OR (input_tokens IS NULL AND output_tokens IS NULL)),
    CONSTRAINT chk_rea_cost_consistent CHECK (
        (cost_kind IN ('measured','estimated')) = (cost_amount IS NOT NULL)),
    CONSTRAINT chk_rea_attempt_number CHECK (attempt_number >= 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

-- ---------------------------------------------------------------------
-- narrative_characters
-- ---------------------------------------------------------------------
--
-- ⚠️ **没有 UNIQUE(name)，而且永远不加**。一本书里可以有两个「老王」；
-- 用名字做唯一键会把他们合成一个人，而合并之后再也分不开。
-- 名字只用于展示和候选查找，身份靠 id。
CREATE TABLE narrative_characters (
    id CHAR(36) NOT NULL,
    job_id CHAR(36) NOT NULL,
    display_name VARCHAR(128) NOT NULL,
    -- source_order 是全书线性位置，用来排序与定位「第一次出现」。
    first_source_order BIGINT NOT NULL,
    -- 至多 2 组简短身份依据。不存人物传记——那会随书变长、无上限，
    -- 且没有任何查询需要它。
    identity_evidence JSON NULL,
    has_ambiguity TINYINT(1) NOT NULL DEFAULT 0,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_nc_job_order (job_id, first_source_order, id),
    CONSTRAINT chk_nc_source_order CHECK (first_source_order >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

-- ---------------------------------------------------------------------
-- narrative_aliases
-- ---------------------------------------------------------------------
CREATE TABLE narrative_aliases (
    id CHAR(36) NOT NULL,
    job_id CHAR(36) NOT NULL,
    -- 只有 state='supported' 才允许指向某个人物；proposed/ambiguous/rejected
    -- 时为 NULL。ambiguous **不自动消解**——猜一个等于制造一条无法追责的合并。
    character_id CHAR(36) NULL,
    surface VARCHAR(128) NOT NULL,
    surface_hash BINARY(32) NOT NULL,
    state VARCHAR(16) NOT NULL DEFAULT 'proposed',
    evidence JSON NOT NULL,
    first_source_order BIGINT NOT NULL,
    -- ⭐ 去重键**必须**包含 mention 来源、候选实体与规则版本，不能只由名字
    -- 组成。只按 surface 去重就是在按名字合并人，正是这张表要防的事。
    decision_key_hash BINARY(32) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_na_job_decision (job_id, decision_key_hash),
    KEY idx_na_character_id (character_id),
    KEY idx_na_job_surface (job_id, surface_hash, state, id),
    CONSTRAINT chk_na_state CHECK (state IN ('proposed','supported','ambiguous','rejected')),
    CONSTRAINT chk_na_supported_has_character CHECK (
        state <> 'supported' OR character_id IS NOT NULL)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

-- ---------------------------------------------------------------------
-- narrative_relations
-- ---------------------------------------------------------------------
--
-- ⭐ 关系**不按当前状态覆盖写**。这是整个功能的立论：关系随剧情变化，
-- 「第 3 回是师徒、第 57 回反目」两条都要在，覆盖写会让全书只剩最后一
-- 个状态，而那恰恰是「人物关系」这类问题最没用的答案。
-- 因此唯一键里含 relation_key_hash（主体/客体/类型/首次出处一起算），
-- 跨章同类型新增记录，同章不同场景也新增。
CREATE TABLE narrative_relations (
    id CHAR(36) NOT NULL,
    job_id CHAR(36) NOT NULL,
    subject_id CHAR(36) NOT NULL,
    object_id CHAR(36) NOT NULL,
    relation_type VARCHAR(64) NOT NULL,
    -- 无向关系按实体 ID 排序后再算 hash，否则 (A,B) 与 (B,A) 会变成两条。
    is_directed TINYINT(1) NOT NULL DEFAULT 1,
    relation_key_hash BINARY(32) NOT NULL,
    first_source_order BIGINT NOT NULL,
    -- ⚠️ 章节号/标题是**首条证据的展示快照**，不是这条关系的唯一故事时间，
    -- 也不能当成排序依据（倒叙的书里章节顺序 ≠ 故事顺序）。排序用
    -- first_source_order（原文位置）。章节未知时为 NULL，不填 0。
    chapter_number INT NULL,
    chapter_title VARCHAR(255) NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_nr_job_key (job_id, relation_key_hash),
    KEY idx_nr_subject_id (subject_id),
    KEY idx_nr_object_id (object_id),
    KEY idx_nr_job_pair (job_id, subject_id, object_id, first_source_order, id),
    CONSTRAINT chk_nr_source_order CHECK (first_source_order >= 0),
    CONSTRAINT chk_nr_chapter_number CHECK (chapter_number IS NULL OR chapter_number >= 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

-- ---------------------------------------------------------------------
-- narrative_relation_evidence
-- ---------------------------------------------------------------------
CREATE TABLE narrative_relation_evidence (
    id CHAR(36) NOT NULL,
    job_id CHAR(36) NOT NULL,
    relation_id CHAR(36) NOT NULL,
    chunk_id CHAR(36) NOT NULL,
    document_version INT NOT NULL,
    source_order BIGINT NOT NULL,
    source_start INT NOT NULL,
    source_end INT NOT NULL,
    quote TEXT NOT NULL,
    source_segments JSON NULL,
    -- ⭐ evidence_key 由**规范化的文档源区间 + quote hash** 算出，
    -- **不以 chunk_id 为唯一性依据**。相邻 chunk 因为 overlap 会包含同一段
    -- 原文，用 chunk_id 去重会把同一处出处记成两条证据，从而虚增
    -- 「证据条数」这个后面要写进报告的数字。
    evidence_key_hash BINARY(32) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_nre_relation_key (relation_id, evidence_key_hash),
    KEY idx_nre_job_id (job_id),
    KEY idx_nre_relation_id (relation_id),
    KEY idx_nre_chunk_id (chunk_id),
    CONSTRAINT chk_nre_range CHECK (source_start >= 0 AND source_start < source_end),
    CONSTRAINT chk_nre_source_order CHECK (source_order >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;
