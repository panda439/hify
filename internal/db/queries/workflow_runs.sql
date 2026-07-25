-- name: CreateWorkflowRun :exec
-- output/error_message have no MySQL-level default (TEXT columns can't
-- carry one) and aren't known yet at run start — literal '' here, filled
-- in for real by FinishWorkflowRun.
INSERT INTO workflow_runs (
    id, workflow_id, status, input, output, error_message, created_by
) VALUES (?, ?, ?, ?, '', '', ?);

-- name: GetWorkflowRunByID :one
SELECT id, workflow_id, status, input, output, error_message, started_at, finished_at, created_by
FROM workflow_runs
WHERE id = ?;

-- name: ListWorkflowRuns :many
SELECT id, workflow_id, status, input, output, error_message, started_at, finished_at, created_by
FROM workflow_runs
WHERE workflow_id = ?
ORDER BY started_at DESC
LIMIT ? OFFSET ?;

-- name: CountWorkflowRuns :one
SELECT COUNT(*) FROM workflow_runs WHERE workflow_id = ?;

-- name: ListWorkflowRunsByCreator :many
-- Non-admin callers only see their own execution history — see
-- workflow.Service.ListRuns for why (runs carry rendered prompts/tool
-- output, unlike the shared workflow definition itself).
SELECT id, workflow_id, status, input, output, error_message, started_at, finished_at, created_by
FROM workflow_runs
WHERE workflow_id = ? AND created_by = ?
ORDER BY started_at DESC
LIMIT ? OFFSET ?;

-- name: CountWorkflowRunsByCreator :one
SELECT COUNT(*) FROM workflow_runs WHERE workflow_id = ? AND created_by = ?;

-- name: FinishWorkflowRun :exec
UPDATE workflow_runs
SET status = ?, output = ?, error_message = ?, finished_at = ?
WHERE id = ?;
