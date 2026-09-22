-- name: InsertContactEvent :one
INSERT INTO contact_events (contact_id, campaign_id, campaign_lead_id, assignment_id, variant_id, send_id, step, type,
                            occurred_at, source, provider_event_id, stage_before, stage_after, data)
VALUES (sqlc.arg('contact_id'), sqlc.narg('campaign_id'), sqlc.narg('campaign_lead_id'), sqlc.narg('assignment_id'),
        sqlc.narg('variant_id'), sqlc.narg('send_id'), sqlc.narg('step'), sqlc.arg('type'), sqlc.arg('occurred_at'),
        sqlc.arg('source'), sqlc.narg('provider_event_id'), sqlc.narg('stage_before'), sqlc.narg('stage_after'),
        sqlc.arg('data'))
ON CONFLICT (provider_event_id, type) WHERE provider_event_id IS NOT NULL DO NOTHING
RETURNING *;

-- name: ListContactEvents :many
SELECT * FROM contact_events WHERE contact_id = $1 ORDER BY occurred_at, id
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountContactEvents :one
SELECT count(*) FROM contact_events WHERE contact_id = $1;

-- name: ListCampaignLeadEvents :many
SELECT * FROM contact_events WHERE campaign_lead_id = $1 ORDER BY occurred_at, id;

-- name: ListActivity :many
SELECT * FROM contact_events
WHERE (sqlc.narg('campaign_id')::uuid IS NULL OR campaign_id = sqlc.narg('campaign_id')::uuid)
  AND (sqlc.narg('contact_id')::uuid IS NULL OR contact_id = sqlc.narg('contact_id')::uuid)
  AND (cardinality(sqlc.arg('types')::text[]) = 0 OR type = ANY(sqlc.arg('types')::text[]))
  AND (sqlc.narg('since')::timestamptz IS NULL OR occurred_at >= sqlc.narg('since')::timestamptz)
ORDER BY occurred_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountActivity :one
SELECT count(*) FROM contact_events
WHERE (sqlc.narg('campaign_id')::uuid IS NULL OR campaign_id = sqlc.narg('campaign_id')::uuid)
  AND (sqlc.narg('contact_id')::uuid IS NULL OR contact_id = sqlc.narg('contact_id')::uuid)
  AND (cardinality(sqlc.arg('types')::text[]) = 0 OR type = ANY(sqlc.arg('types')::text[]))
  AND (sqlc.narg('since')::timestamptz IS NULL OR occurred_at >= sqlc.narg('since')::timestamptz);

-- name: ContactHasEvent :one
SELECT EXISTS (SELECT 1 FROM contact_events WHERE campaign_lead_id = $1 AND type = $2);

-- name: LatestCampaignActivityAt :one
-- Zero time when the campaign has no events: max() is NULL there, and the cast
-- makes sqlc scan the column into a plain time.Time.
SELECT COALESCE(max(occurred_at), '0001-01-01 00:00:00+00'::timestamptz)::timestamptz FROM contact_events WHERE campaign_id = $1;

-- name: CountCampaignLeadsEverReached :many
-- "Ever reached" counts survive a later terminal stage: a contact that replied and
-- then unsubscribed still replied.
SELECT type, count(DISTINCT contact_id)::bigint AS total
FROM contact_events
WHERE (sqlc.narg('campaign_id')::uuid IS NULL OR campaign_id = sqlc.narg('campaign_id')::uuid)
  AND type IN ('sent', 'opened', 'clicked', 'replied', 'interested', 'consent_captured', 'newsletter_eligible',
               'newsletter_pending', 'newsletter_subscribed', 'bounced', 'unsubscribed')
GROUP BY type;
