CREATE TABLE workflows (
    id CHAR(36) NOT NULL,
    name VARCHAR(255) NOT NULL,
    description VARCHAR(1000) NOT NULL DEFAULT '',
    definition JSON NOT NULL,
    is_active TINYINT(1) NOT NULL DEFAULT 1,
    created_by CHAR(36) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_workflows_created_by (created_by)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

-- A run is a job-status row like `documents`, not a pure log table: it's
-- inserted once as status='running' when execution starts and updated once
-- more at the end (status/output/finished_at) — the plan's "同步执行" means
-- both writes happen within the same HTTP request, but it's still a
-- mutation, not an append-only record (see workflow_run_steps below for
-- the append-only counterpart).
CREATE TABLE workflow_runs (
    id CHAR(36) NOT NULL,
    workflow_id CHAR(36) NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'running',
    -- TEXT columns can't carry a MySQL-level DEFAULT (Error 1101) — every
    -- INSERT into this table supplies input/output/error_message
    -- explicitly instead (output/error_message as "" until FinishWorkflowRun
    -- fills them in), same as workflow_run_steps below.
    input TEXT NOT NULL,
    output TEXT NOT NULL,
    error_message TEXT NOT NULL,
    started_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    finished_at DATETIME(3) NULL,
    created_by CHAR(36) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_workflow_runs_workflow_started (workflow_id, started_at DESC),
    CONSTRAINT chk_workflow_runs_status CHECK (status IN ('running', 'succeeded', 'failed'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

-- Pure execution log, one row per completed (or failed) step — same
-- append-only, no-soft-delete convention as `messages`/`chunks` (see
-- CLAUDE.md's "纯记录/日志型表" rule). Written once, after the step
-- finishes; there is no "running" row for an in-progress step, that state
-- only ever exists in-memory + as an SSE event (see executor.go).
CREATE TABLE workflow_run_steps (
    id CHAR(36) NOT NULL,
    workflow_run_id CHAR(36) NOT NULL,
    step_id VARCHAR(64) NOT NULL,
    step_type VARCHAR(32) NOT NULL,
    status VARCHAR(16) NOT NULL,
    input TEXT NOT NULL,
    output TEXT NOT NULL,
    error_message TEXT NOT NULL,
    started_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    finished_at DATETIME(3) NULL,
    PRIMARY KEY (id),
    KEY idx_workflow_run_steps_run_started (workflow_run_id, started_at),
    CONSTRAINT chk_workflow_run_steps_type CHECK (step_type IN ('start', 'end', 'llm_call', 'knowledge_retrieval', 'conditional', 'tool_call')),
    CONSTRAINT chk_workflow_run_steps_status CHECK (status IN ('succeeded', 'failed'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;
