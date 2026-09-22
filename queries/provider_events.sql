-- name: InsertProviderEvent :one
-- The dedupe key makes a redelivery a no-op: no row comes back, and nothing is queued.
INSERT INTO provider_events (id, provider, event_type, dedupe_key, occurred_at, raw, source, campaign_id, contact_id, campaign_lead_id)
VALUES (sqlc.arg('id'), sqlc.arg('provider'), sqlc.arg('event_type'), sqlc.arg('dedupe_key'), sqlc.narg('occurred_at'),
        sqlc.arg('raw'), sqlc.arg('source'), sqlc.narg('campaign_id'), sqlc.narg('contact_id'), sqlc.narg('campaign_lead_id'))
ON CONFLICT (dedupe_key) DO NOTHING
RETURNING *;

-- name: GetProviderEvent :one
SELECT * FROM provider_events WHERE id = $1;

-- name: MarkProviderEventProcessed :exec
UPDATE provider_events
SET processed_at = now(), attempts = attempts + 1, error = sqlc.narg('error'),
    campaign_id = COALESCE(sqlc.narg('campaign_id'), campaign_id),
    contact_id = COALESCE(sqlc.narg('contact_id'), contact_id),
    campaign_lead_id = COALESCE(sqlc.narg('campaign_lead_id'), campaign_lead_id)
WHERE id = sqlc.arg('id');

-- name: MarkProviderEventAttempt :exec
UPDATE provider_events SET attempts = attempts + 1, error = sqlc.narg('error') WHERE id = sqlc.arg('id');

-- name: ResetProviderEvent :one
UPDATE provider_events SET processed_at = NULL, error = NULL WHERE id = $1 RETURNING *;

-- name: ListProviderEvents :many
SELECT * FROM provider_events
WHERE (sqlc.narg('provider')::text IS NULL OR provider = sqlc.narg('provider')::text)
  AND (sqlc.narg('processed')::boolean IS NULL
       OR (sqlc.narg('processed')::boolean = true AND processed_at IS NOT NULL)
       OR (sqlc.narg('processed')::boolean = false AND processed_at IS NULL))
  AND (sqlc.narg('has_error')::boolean IS NULL
       OR (sqlc.narg('has_error')::boolean = true AND error IS NOT NULL)
       OR (sqlc.narg('has_error')::boolean = false AND error IS NULL))
  AND (sqlc.narg('contact_id')::uuid IS NULL OR contact_id = sqlc.narg('contact_id')::uuid)
  AND (sqlc.narg('campaign_id')::uuid IS NULL OR campaign_id = sqlc.narg('campaign_id')::uuid)
ORDER BY received_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountProviderEvents :one
SELECT count(*) FROM provider_events
WHERE (sqlc.narg('provider')::text IS NULL OR provider = sqlc.narg('provider')::text)
  AND (sqlc.narg('processed')::boolean IS NULL
       OR (sqlc.narg('processed')::boolean = true AND processed_at IS NOT NULL)
       OR (sqlc.narg('processed')::boolean = false AND processed_at IS NULL))
  AND (sqlc.narg('has_error')::boolean IS NULL
       OR (sqlc.narg('has_error')::boolean = true AND error IS NOT NULL)
       OR (sqlc.narg('has_error')::boolean = false AND error IS NULL))
  AND (sqlc.narg('contact_id')::uuid IS NULL OR contact_id = sqlc.narg('contact_id')::uuid)
  AND (sqlc.narg('campaign_id')::uuid IS NULL OR campaign_id = sqlc.narg('campaign_id')::uuid);

-- name: CountProviderEventsSummary :one
-- max() is NULL when the provider has no events at all, and the cast makes sqlc
-- scan it into a plain time.Time, so the coalesce keeps that case from failing.
-- The fallback is Go's zero time: callers read it as "never received".
SELECT count(*)::bigint AS total,
       count(*) FILTER (WHERE processed_at IS NULL)::bigint AS unprocessed,
       count(*) FILTER (WHERE error IS NOT NULL)::bigint AS errored,
       COALESCE(max(received_at), '0001-01-01 00:00:00+00'::timestamptz)::timestamptz AS last_received_at
FROM provider_events WHERE provider = $1;
