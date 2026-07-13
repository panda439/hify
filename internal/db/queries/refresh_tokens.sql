-- name: CreateRefreshToken :exec
INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at)
VALUES (?, ?, ?, ?);

-- name: GetRefreshTokenByHash :one
SELECT id, user_id, token_hash, expires_at, revoked_at, created_at
FROM refresh_tokens
WHERE token_hash = ?;

-- name: RevokeRefreshToken :exec
UPDATE refresh_tokens
SET revoked_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND revoked_at IS NULL;

-- name: RevokeAllUserRefreshTokens :exec
UPDATE refresh_tokens
SET revoked_at = CURRENT_TIMESTAMP(3)
WHERE user_id = ? AND revoked_at IS NULL;

-- name: DeleteExpiredRefreshTokens :execrows
DELETE FROM refresh_tokens
WHERE expires_at < CURRENT_TIMESTAMP(3)
   OR (revoked_at IS NOT NULL AND revoked_at < ?);
