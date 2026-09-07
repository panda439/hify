package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"
)

const TaskTypeProcessDocument = "knowledge:process_document"

// TaskTypeReconcileDocuments is registered on the asynq scheduler (see
// cmd/hify/main.go), not enqueued from request-path code — same "no
// payload, fires on a cron schedule" shape as
// auth.TaskTypeCleanupRefreshTokens.
const TaskTypeReconcileDocuments = "knowledge:reconcile_documents"

// TaskTypeReconcileRelationExtractions 与 TaskTypeReconcileDocuments 平行，
// 但**分成两个周期任务**而不是合并：
//   - 文档处理的恢复只碰 documents/chunks，
//   - 抽取的恢复要碰作业、租约和账目；
//
// 合并之后任何一半出错都会让另一半这一轮不跑，而账目那一半停摆的表现是
// 孤儿预留永远停在 reserved——"可能花掉的钱"永远不上账，且毫无症状。
const TaskTypeReconcileRelationExtractions = "knowledge:reconcile_relation_extractions"

// TaskTypeRunRelationExtraction 是**跑一个作业**的任务，由
// StartRelationExtraction 和恢复扫描入队。
//
// ⚠️ 一个作业同一时刻只该有一个 worker 在跑，但队列里出现两条同 job 的任务
// 是正常的（入队时崩了一次、恢复扫描又补了一条）。第二条会在
// claimExtractionJob 那里拿不到租约、安静退出——**并发安全由租约保证，
// 不靠队列去重**。队列去重只能防重复入队，防不住两个 worker 同时开跑。
const TaskTypeRunRelationExtraction = "knowledge:run_relation_extraction"

type runRelationExtractionPayload struct {
	JobID string `json:"job_id"`
}

func newRunRelationExtractionTask(jobID string) (*asynq.Task, error) {
	payload, err := json.Marshal(runRelationExtractionPayload{JobID: jobID})
	if err != nil {
		return nil, fmt.Errorf("knowledge: marshal extraction task payload: %w", err)
	}
	return asynq.NewTask(TaskTypeRunRelationExtraction, payload), nil
}

// NewRelationExtractionTaskHandler 是跑作业的 asynq 适配器。
func NewRelationExtractionTaskHandler(svc Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var payload runRelationExtractionPayload
		if err := json.Unmarshal(t.Payload(), &payload); err != nil {
			return fmt.Errorf("knowledge: unmarshal extraction task payload: %w", err)
		}
		return svc.RunRelationExtraction(ctx, payload.JobID)
	}
}

type processDocumentPayload struct {
	DocumentID string `json:"document_id"`
	// Version is which processing attempt this task instance is
	// authorized to carry out — see Document.Version and
	// Service.ProcessDocument.
	Version int64 `json:"version"`
}

func newProcessDocumentTask(documentID string, version int64) (*asynq.Task, error) {
	payload, err := json.Marshal(processDocumentPayload{DocumentID: documentID, Version: version})
	if err != nil {
		return nil, fmt.Errorf("knowledge: marshal task payload: %w", err)
	}
	return asynq.NewTask(TaskTypeProcessDocument, payload), nil
}

// NewTaskHandler is what cmd/hify/main.go registers on the asynq worker
// mux — a thin adapter from asynq's (ctx, *asynq.Task) error shape to
// Service.ProcessDocument.
func NewTaskHandler(svc Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var payload processDocumentPayload
		if err := json.Unmarshal(t.Payload(), &payload); err != nil {
			return fmt.Errorf("knowledge: unmarshal task payload: %w", err)
		}
		return svc.ProcessDocument(ctx, payload.DocumentID, payload.Version)
	}
}

// NewReconcileTaskHandler is what cmd/hify/main.go registers on the asynq
// worker mux, fired on a periodic schedule (see main.go's scheduler
// setup) rather than enqueued by request-path code — same adapter shape
// as auth.NewCleanupTaskHandler, just no payload to unmarshal.
func NewReconcileTaskHandler(svc Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		n, err := svc.ReconcileStuckDocuments(ctx)
		if err != nil {
			return err
		}
		slog.Info("knowledge: reconciled stuck documents", "reclaimed", n)
		return nil
	}
}

// NewRelationExtractionReconcileHandler 是抽取恢复扫描的 asynq 适配器。
//
// ⚠️ 它只负责把需要接手的作业**重新入队**并把孤儿预留改判；真正的接手在
// TaskTypeRunRelationExtraction 那一侧。恢复扫描必须是个短任务：在里面
// 同步跑几百次模型调用会让下一轮扫描迟迟不来，而所有作业的租约还在滴答。
func NewRelationExtractionReconcileHandler(svc Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		res, err := svc.ReconcileRelationExtractions(ctx)
		if err != nil {
			return err
		}
		if res.JobsNeedingRecovery > 0 || res.ReservationsResolved > 0 {
			slog.Info("knowledge: reconciled relation extractions",
				"jobs_needing_recovery", res.JobsNeedingRecovery,
				"jobs_requeued", res.JobsRequeued,
				"reservations_resolved", res.ReservationsResolved)
		}
		return nil
	}
}
