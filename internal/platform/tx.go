package platform

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// WithTx runs fn inside a MySQL transaction: commits on success, rolls back
// on error or panic (re-panicking after rollback so the caller's recovery
// middleware still sees it). Callers bind their sqlc *gen.Queries to tx via
// queries.WithTx(tx) inside fn — see internal/auth's refresh-token rotation
// for the motivating case: revoking the old token and inserting the new one
// must be atomic, or a crash mid-rotation lets the same refresh token be
// used twice.
func WithTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("platform: begin tx: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(err, fmt.Errorf("platform: rollback also failed: %w", rbErr))
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("platform: commit tx: %w", err)
	}
	return nil
}
