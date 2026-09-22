-- name: UpsertSendingAccount :one
INSERT INTO sending_accounts (id, email, first_name, last_name, provider_code, status, warmup_status, daily_limit,
                              sending_gap, warmup_score, status_message, tracking_domain, tracking_domain_status,
                              setup_pending, is_managed, raw, last_synced_at)
VALUES (sqlc.arg('id'), sqlc.arg('email'), sqlc.narg('first_name'), sqlc.narg('last_name'), sqlc.narg('provider_code'),
        sqlc.arg('status'), sqlc.narg('warmup_status'), sqlc.narg('daily_limit'), sqlc.narg('sending_gap'),
        sqlc.narg('warmup_score'), sqlc.narg('status_message'), sqlc.narg('tracking_domain'),
        sqlc.narg('tracking_domain_status'), sqlc.arg('setup_pending'), sqlc.arg('is_managed'), sqlc.arg('raw'), now())
ON CONFLICT (email) DO UPDATE
SET first_name = EXCLUDED.first_name, last_name = EXCLUDED.last_name, provider_code = EXCLUDED.provider_code,
    status = EXCLUDED.status, warmup_status = EXCLUDED.warmup_status, daily_limit = EXCLUDED.daily_limit,
    sending_gap = EXCLUDED.sending_gap, warmup_score = EXCLUDED.warmup_score, status_message = EXCLUDED.status_message,
    tracking_domain = EXCLUDED.tracking_domain, tracking_domain_status = EXCLUDED.tracking_domain_status,
    setup_pending = EXCLUDED.setup_pending, is_managed = EXCLUDED.is_managed, raw = EXCLUDED.raw,
    last_synced_at = now(), updated_at = now()
RETURNING *;

-- name: GetSendingAccount :one
SELECT * FROM sending_accounts WHERE id = $1;

-- name: GetSendingAccountByEmail :one
SELECT * FROM sending_accounts WHERE email = $1;

-- name: ListSendingAccounts :many
SELECT * FROM sending_accounts
WHERE (sqlc.narg('status')::int IS NULL OR status = sqlc.narg('status')::int)
  AND (sqlc.narg('q')::text IS NULL OR email::text ILIKE '%' || sqlc.narg('q')::text || '%')
ORDER BY email;

-- name: ListSendingAccountsByIDs :many
SELECT * FROM sending_accounts WHERE id = ANY(sqlc.arg('ids')::uuid[]) ORDER BY email;

-- name: UpsertSendingAccountStatsDaily :exec
INSERT INTO sending_account_stats_daily (sending_account_id, day, sent, bounced, contacted, new_leads_contacted,
                                         opened, unique_opened, replies, unique_replies, clicks, unique_clicks, fetched_at)
VALUES (sqlc.arg('sending_account_id'), sqlc.arg('day'), sqlc.arg('sent'), sqlc.arg('bounced'), sqlc.arg('contacted'),
        sqlc.arg('new_leads_contacted'), sqlc.arg('opened'), sqlc.arg('unique_opened'), sqlc.arg('replies'),
        sqlc.arg('unique_replies'), sqlc.arg('clicks'), sqlc.arg('unique_clicks'), now())
ON CONFLICT (sending_account_id, day) DO UPDATE
SET sent = EXCLUDED.sent, bounced = EXCLUDED.bounced, contacted = EXCLUDED.contacted,
    new_leads_contacted = EXCLUDED.new_leads_contacted, opened = EXCLUDED.opened, unique_opened = EXCLUDED.unique_opened,
    replies = EXCLUDED.replies, unique_replies = EXCLUDED.unique_replies, clicks = EXCLUDED.clicks,
    unique_clicks = EXCLUDED.unique_clicks, fetched_at = now();

-- name: ListSendingAccountStatsDaily :many
SELECT * FROM sending_account_stats_daily
WHERE sending_account_id = sqlc.arg('sending_account_id') AND day >= sqlc.arg('since')
ORDER BY day;

-- name: SumSendingAccountStats :many
-- Provider-reported totals per account over a window, for the accounts list.
SELECT sending_account_id,
       COALESCE(sum(sent), 0)::bigint      AS sent,
       COALESCE(sum(bounced), 0)::bigint   AS bounced,
       COALESCE(sum(replies), 0)::bigint   AS replies,
       COALESCE(sum(unique_replies), 0)::bigint AS unique_replies,
       max(day)::date                      AS last_day
FROM sending_account_stats_daily
WHERE day >= sqlc.arg('since')
GROUP BY sending_account_id;
