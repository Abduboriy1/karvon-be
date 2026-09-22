-- name: ListSources :many
SELECT * FROM sources ORDER BY name;

-- name: GetSource :one
SELECT * FROM sources WHERE id = $1;


-- name: UpdateSource :one
UPDATE sources
SET name              = COALESCE(sqlc.narg('name'), name),
    cost_per_1k_cents = COALESCE(sqlc.narg('cost_per_1k_cents'), cost_per_1k_cents),
    enabled           = COALESCE(sqlc.narg('enabled'), enabled),
    api_key_enc       = CASE WHEN sqlc.arg('set_key')::boolean THEN sqlc.narg('api_key_enc')
                             ELSE api_key_enc END,
    updated_at        = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SetSourceTestResult :exec
UPDATE sources
SET last_tested_at = now(),
    last_test_ok   = sqlc.arg('ok'),
    updated_at     = now()
WHERE id = sqlc.arg('id');
