-- name: UpsertEmailSend :one
-- One send per lead per step. A second report of the same send (webhook then
-- reconcile) fills in what was missing rather than creating a duplicate.
INSERT INTO email_sends (id, campaign_lead_id, assignment_id, campaign_id, contact_id, step, variant_id,
                         sending_account_email, sending_account_id, instantly_email_id, provider_message_id,
                         subject_snapshot, sent_at, source)
VALUES (sqlc.arg('id'), sqlc.arg('campaign_lead_id'), sqlc.narg('assignment_id'), sqlc.arg('campaign_id'),
        sqlc.arg('contact_id'), sqlc.arg('step'), sqlc.narg('variant_id'), sqlc.narg('sending_account_email'),
        sqlc.narg('sending_account_id'), sqlc.narg('instantly_email_id'), sqlc.narg('provider_message_id'),
        sqlc.narg('subject_snapshot'), sqlc.arg('sent_at'), sqlc.arg('source'))
ON CONFLICT (campaign_lead_id, step) DO UPDATE
SET assignment_id         = COALESCE(email_sends.assignment_id, EXCLUDED.assignment_id),
    variant_id            = COALESCE(email_sends.variant_id, EXCLUDED.variant_id),
    sending_account_email = COALESCE(email_sends.sending_account_email, EXCLUDED.sending_account_email),
    sending_account_id    = COALESCE(email_sends.sending_account_id, EXCLUDED.sending_account_id),
    instantly_email_id    = COALESCE(email_sends.instantly_email_id, EXCLUDED.instantly_email_id),
    provider_message_id   = COALESCE(email_sends.provider_message_id, EXCLUDED.provider_message_id),
    subject_snapshot      = COALESCE(email_sends.subject_snapshot, EXCLUDED.subject_snapshot),
    sent_at               = LEAST(email_sends.sent_at, EXCLUDED.sent_at),
    updated_at            = now()
RETURNING *;

-- name: GetEmailSend :one
SELECT * FROM email_sends WHERE campaign_lead_id = $1 AND step = $2;

-- name: GetLatestEmailSendForLead :one
SELECT * FROM email_sends WHERE campaign_lead_id = $1 ORDER BY step DESC LIMIT 1;

-- name: ListEmailSendsForLead :many
SELECT * FROM email_sends WHERE campaign_lead_id = $1 ORDER BY step;

-- name: RecordSendOpen :exec
UPDATE email_sends
SET first_opened_at = COALESCE(first_opened_at, sqlc.arg('at')),
    last_opened_at  = GREATEST(COALESCE(last_opened_at, sqlc.arg('at')), sqlc.arg('at')),
    open_count      = open_count + 1,
    updated_at      = now()
WHERE id = sqlc.arg('id');

-- name: RecordSendClick :exec
UPDATE email_sends
SET first_clicked_at = COALESCE(first_clicked_at, sqlc.arg('at')),
    last_clicked_at  = GREATEST(COALESCE(last_clicked_at, sqlc.arg('at')), sqlc.arg('at')),
    click_count      = click_count + 1,
    updated_at       = now()
WHERE id = sqlc.arg('id');

-- name: RecordSendReply :exec
UPDATE email_sends
SET replied_at           = COALESCE(replied_at, sqlc.arg('at')),
    reply_classification = CASE
        WHEN reply_classification IS NULL OR reply_classification IN ('unknown', 'neutral') THEN sqlc.arg('classification')
        ELSE reply_classification END,
    updated_at = now()
WHERE id = sqlc.arg('id');

-- name: SetSendReplyClassification :exec
UPDATE email_sends SET reply_classification = sqlc.arg('classification'), updated_at = now() WHERE id = sqlc.arg('id');

-- name: RecordSendBounce :exec
UPDATE email_sends SET bounced_at = COALESCE(bounced_at, sqlc.arg('at')), updated_at = now() WHERE id = sqlc.arg('id');

-- name: RecordSendUnsubscribe :exec
UPDATE email_sends SET unsubscribed_at = COALESCE(unsubscribed_at, sqlc.arg('at')), updated_at = now() WHERE id = sqlc.arg('id');

-- name: LatestSendTimestampForCampaign :one
-- Zero time when the campaign has no sends: max() is NULL there, and the cast
-- makes sqlc scan the column into a plain time.Time.
SELECT COALESCE(max(sent_at), '0001-01-01 00:00:00+00'::timestamptz)::timestamptz FROM email_sends WHERE campaign_id = $1;
