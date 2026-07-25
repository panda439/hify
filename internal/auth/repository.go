package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"hify/internal/db/gen"
	"hify/internal/platform"
)

// Repository is constructed via NewRepository in wire.go.
type Repository struct {
	db      *sql.DB
	queries *gen.Queries
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func insertRefreshToken(ctx context.Context, q *gen.Queries, userID, rawToken string, ttl time.Duration) error {
	err := q.CreateRefreshToken(ctx, gen.CreateRefreshTokenParams{
		ID:        platform.NewID(),
		UserID:    userID,
		TokenHash: hashToken(rawToken),
		ExpiresAt: time.Now().Add(ttl),
	})
	if err != nil {
		return fmt.Errorf("auth: insert refresh token: %w", err)
	}
	return nil
}

func (r *Repository) create(ctx context.Context, userID, rawToken string, ttl time.Duration) error {
	return insertRefreshToken(ctx, r.queries, userID, rawToken, ttl)
}

// getValidByToken looks up rawToken and rejects it (as ErrInvalidRefreshToken)
// if it doesn't exist, is expired, or was already revoked — the caller
// can't tell which, by design, since that distinction isn't useful to a
// client and would just be another thing to get wrong.
func (r *Repository) getValidByToken(ctx context.Context, rawToken string) (refreshTokenRecord, error) {
	row, err := r.queries.GetRefreshTokenByHash(ctx, hashToken(rawToken))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return refreshTokenRecord{}, ErrInvalidRefreshToken
		}
		return refreshTokenRecord{}, fmt.Errorf("auth: get refresh token: %w", err)
	}
	if row.RevokedAt.Valid || time.Now().After(row.ExpiresAt) {
		return refreshTokenRecord{}, ErrInvalidRefreshToken
	}
	return refreshTokenRecord{ID: row.ID, UserID: row.UserID}, nil
}

// rotate revokes oldID and inserts a new refresh token for userID as a
// single atomic transaction — without this, a crash between the revoke and
// the insert would either resurrect a revoked token or lose the session
// entirely (see CLAUDE.md's WithTx rationale).
func (r *Repository) rotate(ctx context.Context, oldID, userID, newRawToken string, ttl time.Duration) error {
	return platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)
		if err := q.RevokeRefreshToken(ctx, oldID); err != nil {
			return fmt.Errorf("auth: revoke old refresh token: %w", err)
		}
		if err := insertRefreshToken(ctx, q, userID, newRawToken, ttl); err != nil {
			return err
		}
		return nil
	})
}

func (r *Repository) revoke(ctx context.Context, id string) error {
	if err := r.queries.RevokeRefreshToken(ctx, id); err != nil {
		return fmt.Errorf("auth: revoke refresh token: %w", err)
	}
	return nil
}

// deleteExpired purges rows past their expiry, plus rows revoked before
// revokedBefore — the periodic cleanup CLAUDE.md requires for this
// TTL-semantic table (see tasks.go's cleanup task).
func (r *Repository) deleteExpired(ctx context.Context, revokedBefore time.Time) (int64, error) {
	n, err := r.queries.DeleteExpiredRefreshTokens(ctx, sql.NullTime{Time: revokedBefore, Valid: true})
	if err != nil {
		return 0, fmt.Errorf("auth: delete expired refresh tokens: %w", err)
	}
	return n, nil
}
