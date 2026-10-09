-- name: UpsertCampaignAnalyticsSnapshot :exec
INSERT INTO campaign_analytics_snapshots (id, campaign_id, day, source, metrics, fetched_at)
VALUES (sqlc.arg('id'), sqlc.arg('campaign_id'), sqlc.arg('day'), sqlc.arg('source'), sqlc.arg('metrics'), now())
ON CONFLICT (campaign_id, day, source) DO UPDATE SET metrics = EXCLUDED.metrics, fetched_at = now();

-- name: LatestCampaignAnalyticsSnapshot :one
SELECT * FROM campaign_analytics_snapshots WHERE campaign_id = $1 AND source = $2 ORDER BY day DESC LIMIT 1;

-- name: ListCampaignAnalyticsSnapshots :many
SELECT * FROM campaign_analytics_snapshots
WHERE campaign_id = sqlc.arg('campaign_id') AND source = sqlc.arg('source') AND day >= sqlc.arg('since')
ORDER BY day;

-- name: UpsertCampaignStatsDaily :exec
INSERT INTO campaign_stats_daily (campaign_id, day, sent, contacted, new_leads_contacted, opened, unique_opened,
                                  replies, unique_replies, clicks, unique_clicks, opportunities, fetched_at)
VALUES (sqlc.arg('campaign_id'), sqlc.arg('day'), sqlc.arg('sent'), sqlc.arg('contacted'), sqlc.arg('new_leads_contacted'),
        sqlc.arg('opened'), sqlc.arg('unique_opened'), sqlc.arg('replies'), sqlc.arg('unique_replies'),
        sqlc.arg('clicks'), sqlc.arg('unique_clicks'), sqlc.arg('opportunities'), now())
ON CONFLICT (campaign_id, day) DO UPDATE
SET sent = EXCLUDED.sent, contacted = EXCLUDED.contacted, new_leads_contacted = EXCLUDED.new_leads_contacted,
    opened = EXCLUDED.opened, unique_opened = EXCLUDED.unique_opened, replies = EXCLUDED.replies,
    unique_replies = EXCLUDED.unique_replies, clicks = EXCLUDED.clicks, unique_clicks = EXCLUDED.unique_clicks,
    opportunities = EXCLUDED.opportunities, fetched_at = now();

-- name: LastCampaignStatsDay :one
-- The latest day held for a campaign; NULL before its first daily fetch.
SELECT max(day)::date FROM campaign_stats_daily WHERE campaign_id = $1;

-- name: CreateSyncRun :one
INSERT INTO sync_runs (id, kind, target_id) VALUES (sqlc.arg('id'), sqlc.arg('kind'), sqlc.narg('target_id')) RETURNING *;

-- name: FinishSyncRun :exec
UPDATE sync_runs
SET status = sqlc.arg('status'), finished_at = now(), items_seen = sqlc.arg('items_seen'),
    items_updated = sqlc.arg('items_updated'), error = sqlc.narg('error'), details = sqlc.arg('details')
WHERE id = sqlc.arg('id');

-- name: ListSyncRuns :many
SELECT * FROM sync_runs
WHERE (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind')::text)
ORDER BY started_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountSyncRuns :one
SELECT count(*) FROM sync_runs WHERE (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind')::text);

-- name: LastSyncRun :one
SELECT * FROM sync_runs WHERE kind = $1 AND status = 'done' ORDER BY finished_at DESC LIMIT 1;
