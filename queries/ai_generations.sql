-- name: CreateAIGeneration :one
INSERT INTO ai_generations (id, provider, model, status, campaign_id, brief, prompt, prompt_version)
VALUES (sqlc.arg('id'), sqlc.arg('provider'), sqlc.narg('model'), sqlc.arg('status'), sqlc.narg('campaign_id'),
        sqlc.arg('brief'), sqlc.arg('prompt'), sqlc.arg('prompt_version'))
RETURNING *;

-- name: GetAIGeneration :one
SELECT * FROM ai_generations WHERE id = $1;

-- name: ListAIGenerations :many
SELECT * FROM ai_generations
WHERE (sqlc.narg('campaign_id')::uuid IS NULL OR campaign_id = sqlc.narg('campaign_id')::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountAIGenerations :one
SELECT count(*) FROM ai_generations
WHERE (sqlc.narg('campaign_id')::uuid IS NULL OR campaign_id = sqlc.narg('campaign_id')::uuid);

-- name: SetAIGenerationParsed :one
UPDATE ai_generations
SET status = 'parsed', raw_output = sqlc.arg('raw_output'), parsed = sqlc.arg('parsed'),
    component_count = sqlc.arg('component_count'), variant_count = sqlc.arg('variant_count'),
    usage = COALESCE(sqlc.narg('usage'), usage), model = COALESCE(sqlc.narg('model'), model),
    error = NULL, parsed_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SetAIGenerationImported :one
UPDATE ai_generations
SET status = 'imported', imported_component_ids = sqlc.arg('component_ids'), imported_variant_ids = sqlc.arg('variant_ids'),
    imported_at = now()
WHERE id = sqlc.arg('id') AND status = 'parsed'
RETURNING *;

-- name: SetAIGenerationFailed :exec
UPDATE ai_generations SET status = 'failed', error = sqlc.narg('error') WHERE id = sqlc.arg('id');
