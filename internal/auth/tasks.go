package auth

import (
	"context"
	"log/slog"

	"github.com/hibiken/asynq"
)

const TaskTypeCleanupRefreshTokens = "auth:cleanup_refresh_tokens"

// NewCleanupTaskHandler is what cmd/hify/main.go registers on the asynq
// worker mux, fired on a periodic schedule (see main.go's scheduler setup)
// rather than enqueued by request-path code — same adapter shape as
// knowledge.NewTaskHandler, just no payload to unmarshal.
func NewCleanupTaskHandler(svc Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		n, err := svc.CleanupExpiredRefreshTokens(ctx)
		if err != nil {
			return err
		}
		slog.Info("auth: cleaned up refresh_tokens", "rows_deleted", n)
		return nil
	}
}
