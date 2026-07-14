-- name: CreateWorkflowRunStep :exec
INSERT INTO workflow_run_steps (
    id, workflow_run_id, step_id, step_type, status, input, output, error_message, started_at, finished_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListWorkflowRunSteps :many
SELECT id, workflow_run_id, step_id, step_type, status, input, output, error_message, started_at, finished_at
FROM workflow_run_steps
WHERE workflow_run_id = ?
ORDER BY started_at ASC;
