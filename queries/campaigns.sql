-- name: CreateCampaign :one
INSERT INTO campaigns (id, name, brief, schedule, settings, steps, step_delays)
VALUES (sqlc.arg('id'), sqlc.arg('name'), sqlc.arg('brief'), sqlc.arg('schedule'), sqlc.arg('settings'),
        sqlc.arg('steps'), sqlc.arg('step_delays'))
RETURNING *;

-- name: GetCampaign :one
SELECT * FROM campaigns WHERE id = $1;

-- name: GetCampaignByInstantlyID :one
SELECT * FROM campaigns WHERE instantly_campaign_id = $1;

-- name: UpdateCampaign :one
UPDATE campaigns
SET name        = COALESCE(sqlc.narg('name'), name),
    brief       = COALESCE(sqlc.narg('brief'), brief),
    schedule    = COALESCE(sqlc.narg('schedule'), schedule),
    settings    = COALESCE(sqlc.narg('settings'), settings),
    steps       = COALESCE(sqlc.narg('steps'), steps),
    step_delays = COALESCE(sqlc.narg('step_delays'), step_delays),
    updated_at  = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SetCampaignStatus :one
UPDATE campaigns
SET status     = sqlc.arg('status'),
    error      = sqlc.narg('error'),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: ClaimCampaignLaunch :one
-- A compare-and-set: only a ready or scheduled campaign can start launching, and
-- only once.
UPDATE campaigns
SET status = 'launching', launch_claimed_at = now(), error = NULL, updated_at = now()
WHERE id = $1 AND status IN ('ready', 'scheduled')
RETURNING *;

-- name: MarkCampaignReady :one
-- A launch now. It also takes over a scheduled campaign, which is how "launch now
-- instead" works; the new request id makes the scheduled job stale.
UPDATE campaigns
SET status = 'ready', scheduled_launch_at = NULL, launch_request_id = sqlc.arg('launch_request_id'),
    error = NULL, updated_at = now()
WHERE id = sqlc.arg('id') AND status IN ('draft', 'ready', 'failed', 'scheduled')
RETURNING *;

-- name: ScheduleCampaignLaunch :one
-- A launch later, or a new time for one already scheduled.
UPDATE campaigns
SET status = 'scheduled', scheduled_launch_at = sqlc.arg('scheduled_launch_at'),
    launch_request_id = sqlc.arg('launch_request_id'), error = NULL, updated_at = now()
WHERE id = sqlc.arg('id') AND status IN ('draft', 'ready', 'failed', 'scheduled')
RETURNING *;

-- name: UnscheduleCampaign :one
UPDATE campaigns
SET status = 'draft', scheduled_launch_at = NULL, launch_request_id = NULL, updated_at = now()
WHERE id = $1 AND status = 'scheduled'
RETURNING *;

-- name: SetCampaignInstantlyID :one
UPDATE campaigns
SET instantly_campaign_id = sqlc.arg('instantly_campaign_id'), updated_at = now()
WHERE id = sqlc.arg('id') AND instantly_campaign_id IS NULL
RETURNING *;

-- name: MarkCampaignActive :one
UPDATE campaigns
SET status = 'active', launched_at = COALESCE(launched_at, now()), paused_at = NULL, error = NULL, updated_at = now()
WHERE id = $1 AND status IN ('launching', 'paused', 'active')
RETURNING *;

-- name: MarkCampaignPaused :one
UPDATE campaigns
SET status = 'paused', paused_at = now(), updated_at = now()
WHERE id = $1 AND status IN ('active', 'launching')
RETURNING *;

-- name: MarkCampaignCompleted :one
UPDATE campaigns
SET status = 'completed', completed_at = COALESCE(completed_at, now()), updated_at = now()
WHERE id = $1 AND status IN ('active', 'paused')
RETURNING *;

-- name: MarkCampaignFailed :one
UPDATE campaigns
SET status = 'failed', error = sqlc.narg('error'), updated_at = now()
WHERE id = sqlc.arg('id') AND status IN ('ready', 'scheduled', 'launching')
RETURNING *;

-- name: ArchiveCampaign :one
UPDATE campaigns
SET status = 'archived', archived_at = now(), updated_at = now()
WHERE id = $1 AND status <> 'archived'
RETURNING *;

-- name: SetCampaignProviderState :exec
-- synced_at comes from the workers' clock, not now(): the sent-email backfill
-- compares it with Instantly's timestamps, so both must be on the same clock.
UPDATE campaigns
SET instantly_status             = sqlc.narg('instantly_status'),
    instantly_sending_status     = sqlc.narg('sending_status'),
    instantly_not_sending_status = sqlc.narg('not_sending_status'),
    last_synced_at               = sqlc.arg('synced_at'),
    last_sync_error              = NULL,
    updated_at                   = now()
WHERE id = sqlc.arg('id');

-- name: SetCampaignSyncError :exec
UPDATE campaigns SET last_sync_error = sqlc.narg('error'), updated_at = now() WHERE id = sqlc.arg('id');

-- name: BumpCampaignWeightsVersion :one
UPDATE campaigns SET weights_version = weights_version + 1, updated_at = now() WHERE id = $1 RETURNING weights_version;

-- name: RecomputeCampaignCounts :one
UPDATE campaigns c
SET leads_total  = counted.total,
    leads_pushed = counted.pushed
FROM (SELECT count(*)::int AS total,
             count(*) FILTER (WHERE pushed_at IS NOT NULL)::int AS pushed
      FROM campaign_leads WHERE campaign_id = sqlc.arg('id')) counted
WHERE c.id = sqlc.arg('id')
RETURNING c.*;

-- name: ListCampaignsByStatus :many
SELECT * FROM campaigns WHERE status = ANY(sqlc.arg('statuses')::text[]) ORDER BY created_at;

-- name: ListFinishedImportedCampaignsToSync :many
-- Campaigns started in Instantly that have stopped sending but still need a sync:
-- one that has never had its per-day figures fetched, or one completed recently
-- enough that replies and opportunities may still be coming in.
SELECT * FROM campaigns c
WHERE c.source = 'instantly' AND c.instantly_campaign_id IS NOT NULL
  AND c.status NOT IN ('active', 'paused', 'launching', 'archived')
  AND (NOT EXISTS (SELECT 1 FROM campaign_stats_daily d WHERE d.campaign_id = c.id)
       OR c.completed_at >= now() - make_interval(days => sqlc.arg('settle_days')::int))
ORDER BY c.created_at;

-- name: CountCampaignsByStatus :many
SELECT status, count(*)::bigint AS total FROM campaigns GROUP BY status;

-- name: SetCampaignSendingAccounts :exec
DELETE FROM campaign_sending_accounts WHERE campaign_id = $1;

-- name: AddCampaignSendingAccount :exec
INSERT INTO campaign_sending_accounts (campaign_id, sending_account_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: ListCampaignSendingAccounts :many
SELECT sa.* FROM sending_accounts sa
JOIN campaign_sending_accounts csa ON csa.sending_account_id = sa.id
WHERE csa.campaign_id = $1
ORDER BY sa.email;

-- name: ListCampaignsForSendingAccount :many
SELECT c.* FROM campaigns c
JOIN campaign_sending_accounts csa ON csa.campaign_id = c.id
WHERE csa.sending_account_id = $1
ORDER BY c.created_at DESC;

-- name: UpsertInstantlyCampaign :one
-- Imports a campaign started in Instantly, or refreshes one imported before. A
-- campaign Karvon launched matches on its Instantly id too, but is left alone
-- (no row comes back): its own sync owns it. An archived import stays archived.
INSERT INTO campaigns (id, name, status, steps, source, instantly_campaign_id, instantly_status,
                       launched_at, paused_at, completed_at, created_at, updated_at)
VALUES (sqlc.arg('id'), sqlc.arg('name'), sqlc.arg('status'), sqlc.arg('steps'), 'instantly',
        sqlc.arg('instantly_campaign_id'), sqlc.arg('instantly_status'),
        CASE WHEN sqlc.arg('status')::text <> 'draft' THEN sqlc.arg('created_at')::timestamptz END,
        CASE WHEN sqlc.arg('status')::text = 'paused' THEN now() END,
        CASE WHEN sqlc.arg('status')::text = 'completed' THEN now() END,
        sqlc.arg('created_at'), now())
ON CONFLICT (instantly_campaign_id) DO UPDATE
SET name            = EXCLUDED.name,
    steps           = EXCLUDED.steps,
    instantly_status = EXCLUDED.instantly_status,
    status          = CASE WHEN campaigns.status = 'archived' THEN campaigns.status ELSE EXCLUDED.status END,
    launched_at     = COALESCE(campaigns.launched_at, EXCLUDED.launched_at),
    paused_at       = CASE WHEN EXCLUDED.status = 'paused' THEN COALESCE(campaigns.paused_at, now()) END,
    completed_at    = CASE WHEN EXCLUDED.status = 'completed' THEN COALESCE(campaigns.completed_at, now()) END,
    updated_at      = CASE WHEN (campaigns.name, campaigns.steps, campaigns.status, campaigns.instantly_status)
                                IS DISTINCT FROM (EXCLUDED.name, EXCLUDED.steps, EXCLUDED.status, EXCLUDED.instantly_status)
                           THEN now() ELSE campaigns.updated_at END
WHERE campaigns.source = 'instantly'
RETURNING *, (xmax = 0)::boolean AS inserted;

-- name: SetImportedCampaignCounts :exec
-- An imported campaign's leads live only in Instantly, so its counters come from
-- Instantly's analytics rather than from campaign_leads.
UPDATE campaigns
SET leads_total = sqlc.arg('leads_total'), leads_pushed = sqlc.arg('leads_total')
WHERE id = sqlc.arg('id') AND source = 'instantly';
