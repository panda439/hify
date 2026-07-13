-- name: CreateUser :exec
INSERT INTO users (id, email, password_hash, display_name, role, is_active)
VALUES (?, ?, ?, ?, ?, ?);

-- name: GetUserByID :one
SELECT id, email, password_hash, display_name, role, is_active, created_at, updated_at
FROM users
WHERE id = ?;

-- name: GetUserByEmail :one
SELECT id, email, password_hash, display_name, role, is_active, created_at, updated_at
FROM users
WHERE email = ?;

-- name: ListUsers :many
SELECT id, email, password_hash, display_name, role, is_active, created_at, updated_at
FROM users
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: UpdateUser :exec
UPDATE users
SET display_name = ?, role = ?, is_active = ?
WHERE id = ?;

-- name: UpdateUserPassword :exec
UPDATE users
SET password_hash = ?
WHERE id = ?;
