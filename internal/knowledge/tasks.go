package knowledge

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
)

const TaskTypeProcessDocument = "knowledge:process_document"

type processDocumentPayload struct {
	DocumentID string `json:"document_id"`
}

func newProcessDocumentTask(documentID string) (*asynq.Task, error) {
	payload, err := json.Marshal(processDocumentPayload{DocumentID: documentID})
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
		return svc.ProcessDocument(ctx, payload.DocumentID)
	}
}
