// Package platform holds cross-cutting infrastructure shared by every module:
// logging, DB/Redis/asynq clients, ID generation, pagination helpers and the
// apperr/httperr error-classification layer.
package platform

import (
	"log/slog"
	"os"
)

func NewLogger(env string) *slog.Logger {
	level := slog.LevelInfo
	if env == "development" {
		level = slog.LevelDebug
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}
