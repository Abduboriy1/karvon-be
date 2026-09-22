-- name: GetCampaignSettings :one
SELECT * FROM campaign_settings WHERE id = 1;

-- name: SetInstantlyWebhook :one
UPDATE campaign_settings
SET instantly_webhook_token      = sqlc.narg('token'),
    instantly_webhook_secret_enc = sqlc.narg('secret_enc'),
    instantly_webhook_id         = sqlc.narg('webhook_id'),
    instantly_webhook_url        = sqlc.narg('webhook_url'),
    instantly_webhook_status     = sqlc.narg('status'),
    instantly_webhook_error      = sqlc.narg('error'),
    updated_at                   = now()
WHERE id = 1
RETURNING *;

-- name: SetInstantlyWebhookStatus :exec
UPDATE campaign_settings
SET instantly_webhook_status = sqlc.narg('status'),
    instantly_webhook_error  = sqlc.narg('error'),
    updated_at               = now()
WHERE id = 1;

-- name: SetDefaultAudience :exec
UPDATE campaign_settings SET default_audience_id = sqlc.narg('audience_id'), updated_at = now() WHERE id = 1;
