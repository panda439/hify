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
