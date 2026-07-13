-- name: CreateProvider :exec
INSERT INTO model_providers (
    id, name, adapter_type, base_url, auth_type, api_key_encrypted,
    extra_headers, extra_config, created_by
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetProviderByID :one
SELECT id, name, adapter_type, base_url, auth_type, api_key_encrypted,
       extra_headers, extra_config, last_tested_at, last_test_status,
       last_test_error, is_active, created_by, created_at, updated_at
FROM model_providers
WHERE id = ?;

-- name: ListProviders :many
SELECT id, name, adapter_type, base_url, auth_type, api_key_encrypted,
       extra_headers, extra_config, last_tested_at, last_test_status,
       last_test_error, is_active, created_by, created_at, updated_at
FROM model_providers
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountProviders :one
SELECT COUNT(*) FROM model_providers;

-- name: UpdateProvider :exec
UPDATE model_providers
SET name = ?, base_url = ?, auth_type = ?, extra_headers = ?, extra_config = ?, is_active = ?
WHERE id = ?;

-- name: UpdateProviderAPIKey :exec
UPDATE model_providers
SET api_key_encrypted = ?
WHERE id = ?;

-- name: UpdateProviderTestResult :exec
UPDATE model_providers
SET last_tested_at = ?, last_test_status = ?, last_test_error = ?
WHERE id = ?;
