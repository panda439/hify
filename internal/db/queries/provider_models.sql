-- name: CreateProviderModel :exec
INSERT INTO provider_models (
    id, provider_id, model_name, capability, context_window,
    max_output_tokens, embedding_dimension, is_default
) VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetProviderModelByID :one
SELECT id, provider_id, model_name, capability, context_window, max_output_tokens,
       embedding_dimension, is_default, is_active, created_at, updated_at
FROM provider_models
WHERE id = ?;

-- name: ListProviderModelsByProvider :many
SELECT id, provider_id, model_name, capability, context_window, max_output_tokens,
       embedding_dimension, is_default, is_active, created_at, updated_at
FROM provider_models
WHERE provider_id = ?
ORDER BY capability, model_name;

-- name: ListActiveModelsByCapability :many
SELECT pm.id, pm.provider_id, pm.model_name, pm.capability, pm.context_window,
       pm.max_output_tokens, pm.embedding_dimension, pm.is_default, pm.is_active,
       pm.created_at, pm.updated_at
FROM provider_models pm
INNER JOIN model_providers p ON p.id = pm.provider_id
WHERE pm.capability = ? AND pm.is_active = 1 AND p.is_active = 1
ORDER BY pm.model_name;

-- name: UpdateProviderModel :exec
UPDATE provider_models
SET context_window = ?, max_output_tokens = ?, embedding_dimension = ?,
    is_default = ?, is_active = ?
WHERE id = ?;
