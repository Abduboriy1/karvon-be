-- name: UpsertInboxEmail :one
-- Instantly's email id is the key, so a re-sync overwrites rather than duplicates.
-- The read flag only ever moves from unread to read here: once an operator has read
-- a thread in Karvon, a stale "unread" from an older page must not undo it.
INSERT INTO inbox_emails (id, thread_id, message_id, direction, ue_type, email_account, lead_email, from_address,
                          to_addresses, cc_addresses, subject, body_text, body_html, content_preview, step,
                          is_unread, is_auto_reply, interest_status, ai_interest_value, instantly_campaign_id,
                          campaign_id, contact_id, sent_at, provider_created_at)
VALUES (sqlc.arg('id'), sqlc.narg('thread_id'), sqlc.narg('message_id'), sqlc.arg('direction'), sqlc.narg('ue_type'),
        sqlc.narg('email_account'), sqlc.narg('lead_email'), sqlc.narg('from_address'), sqlc.narg('to_addresses'),
        sqlc.narg('cc_addresses'), sqlc.narg('subject'), sqlc.narg('body_text'), sqlc.narg('body_html'),
        sqlc.narg('content_preview'), sqlc.narg('step'), sqlc.arg('is_unread'), sqlc.arg('is_auto_reply'),
        sqlc.narg('interest_status'), sqlc.narg('ai_interest_value'), sqlc.narg('instantly_campaign_id'),
        sqlc.narg('campaign_id'), sqlc.narg('contact_id'), sqlc.arg('sent_at'), sqlc.arg('provider_created_at'))
ON CONFLICT (id) DO UPDATE
SET thread_id = EXCLUDED.thread_id, message_id = EXCLUDED.message_id, ue_type = EXCLUDED.ue_type,
    email_account = EXCLUDED.email_account, lead_email = EXCLUDED.lead_email, from_address = EXCLUDED.from_address,
    to_addresses = EXCLUDED.to_addresses, cc_addresses = EXCLUDED.cc_addresses, subject = EXCLUDED.subject,
    body_text = COALESCE(EXCLUDED.body_text, inbox_emails.body_text),
    body_html = COALESCE(EXCLUDED.body_html, inbox_emails.body_html),
    content_preview = EXCLUDED.content_preview, step = EXCLUDED.step,
    is_unread = inbox_emails.is_unread AND EXCLUDED.is_unread,
    is_auto_reply = EXCLUDED.is_auto_reply, interest_status = EXCLUDED.interest_status,
    ai_interest_value = EXCLUDED.ai_interest_value, instantly_campaign_id = EXCLUDED.instantly_campaign_id,
    campaign_id = EXCLUDED.campaign_id, contact_id = EXCLUDED.contact_id, sent_at = EXCLUDED.sent_at,
    provider_created_at = EXCLUDED.provider_created_at, updated_at = now()
RETURNING (xmax = 0)::bool AS inserted;

-- name: InboxWatermark :one
-- The newest received email already mirrored, by Instantly's creation time, which
-- is the order the incremental sync walks in. The epoch means "nothing yet".
SELECT COALESCE(max(provider_created_at), 'epoch')::timestamptz AS watermark
FROM inbox_emails
WHERE direction = 'received';

-- name: ListInboxThread :many
SELECT * FROM inbox_emails WHERE thread_id = $1 ORDER BY sent_at, id;

-- name: CountInboxThreadSent :one
SELECT count(*)::bigint FROM inbox_emails WHERE thread_id = $1 AND direction = 'sent';

-- name: MarkInboxThreadRead :execrows
UPDATE inbox_emails SET is_unread = false, updated_at = now()
WHERE thread_id = $1 AND is_unread;

-- name: CountInboxUnread :one
SELECT count(*)::bigint FROM inbox_emails WHERE direction = 'received' AND is_unread;
