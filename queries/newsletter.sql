-- name: UpsertNewsletterAudience :one
INSERT INTO newsletter_audiences (id, mailchimp_list_id, name, double_optin, member_count, stats, last_synced_at)
VALUES (sqlc.arg('id'), sqlc.arg('mailchimp_list_id'), sqlc.arg('name'), sqlc.arg('double_optin'),
        sqlc.narg('member_count'), sqlc.arg('stats'), now())
ON CONFLICT (mailchimp_list_id) DO UPDATE
SET name = EXCLUDED.name, double_optin = EXCLUDED.double_optin, member_count = EXCLUDED.member_count,
    stats = EXCLUDED.stats, last_synced_at = now(), last_sync_error = NULL, updated_at = now()
RETURNING *;

-- name: GetNewsletterAudience :one
SELECT * FROM newsletter_audiences WHERE id = $1;

-- name: GetNewsletterAudienceByListID :one
SELECT * FROM newsletter_audiences WHERE mailchimp_list_id = $1;

-- name: GetNewsletterAudienceByToken :one
SELECT * FROM newsletter_audiences WHERE webhook_token = $1;

-- name: GetDefaultNewsletterAudience :one
SELECT * FROM newsletter_audiences WHERE is_default LIMIT 1;

-- name: ListNewsletterAudiences :many
SELECT * FROM newsletter_audiences ORDER BY is_default DESC, name;

-- name: UpdateNewsletterAudienceSettings :one
UPDATE newsletter_audiences
SET allow_single_opt_in = COALESCE(sqlc.narg('allow_single_opt_in'), allow_single_opt_in),
    default_tags        = COALESCE(sqlc.narg('default_tags'), default_tags),
    updated_at          = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: ClearDefaultNewsletterAudience :exec
UPDATE newsletter_audiences SET is_default = false, updated_at = now() WHERE is_default;

-- name: SetDefaultNewsletterAudience :one
UPDATE newsletter_audiences SET is_default = true, updated_at = now() WHERE id = $1 RETURNING *;

-- name: SetNewsletterAudienceWebhook :one
UPDATE newsletter_audiences
SET webhook_token = sqlc.narg('token'), webhook_id = sqlc.narg('webhook_id'),
    webhook_secret_enc = sqlc.narg('secret_enc'), webhook_url = sqlc.narg('webhook_url'), updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SetNewsletterAudienceSyncError :exec
UPDATE newsletter_audiences SET last_sync_error = sqlc.narg('error'), updated_at = now() WHERE id = sqlc.arg('id');

-- name: CreateNewsletterSubscription :one
INSERT INTO newsletter_subscriptions (id, contact_id, audience_id, consent_id, requested_status, subscriber_hash, tags)
VALUES (sqlc.arg('id'), sqlc.arg('contact_id'), sqlc.arg('audience_id'), sqlc.narg('consent_id'),
        sqlc.arg('requested_status'), sqlc.arg('subscriber_hash'), sqlc.arg('tags'))
ON CONFLICT (contact_id, audience_id) DO NOTHING
RETURNING *;

-- name: GetNewsletterSubscription :one
SELECT * FROM newsletter_subscriptions WHERE id = $1;

-- name: GetNewsletterSubscriptionByContact :one
SELECT * FROM newsletter_subscriptions WHERE contact_id = $1 AND audience_id = $2;

-- name: GetNewsletterSubscriptionByHash :one
SELECT * FROM newsletter_subscriptions WHERE audience_id = $1 AND subscriber_hash = $2;

-- name: ListNewsletterSubscriptionsForContact :many
SELECT * FROM newsletter_subscriptions WHERE contact_id = $1 ORDER BY created_at;

-- name: ClaimNewsletterSubscription :one
-- Compare-and-set: only a queued or failed subscription can be picked up.
UPDATE newsletter_subscriptions
SET sync_status = 'syncing', claimed_at = now(), sync_attempts = sync_attempts + 1, updated_at = now()
WHERE id = $1 AND sync_status IN ('queued', 'failed')
RETURNING *;

-- name: RequeueNewsletterSubscription :one
UPDATE newsletter_subscriptions
SET sync_status = 'queued', claimed_at = NULL, last_error = sqlc.narg('error'),
    requested_status = COALESCE(sqlc.narg('requested_status'), requested_status), updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: FailNewsletterSubscription :exec
UPDATE newsletter_subscriptions
SET sync_status = 'failed', claimed_at = NULL, last_error = sqlc.narg('error'), updated_at = now()
WHERE id = sqlc.arg('id');

-- name: MarkNewsletterSubscriptionSynced :one
UPDATE newsletter_subscriptions
SET sync_status = 'synced', claimed_at = NULL, last_error = NULL, pushed_at = COALESCE(pushed_at, now()),
    last_synced_at = now(), status = sqlc.arg('status'), requested_status = sqlc.arg('requested_status'),
    subscriber_hash = COALESCE(sqlc.narg('subscriber_hash'), subscriber_hash),
    unique_email_id = COALESCE(sqlc.narg('unique_email_id'), unique_email_id),
    mailchimp_contact_id = COALESCE(sqlc.narg('mailchimp_contact_id'), mailchimp_contact_id),
    web_id = COALESCE(sqlc.narg('web_id'), web_id),
    subscribed_at = CASE WHEN sqlc.arg('status') = 'subscribed' THEN COALESCE(subscribed_at, now()) ELSE subscribed_at END,
    unsubscribed_at = CASE WHEN sqlc.arg('status') IN ('unsubscribed', 'cleaned') THEN COALESCE(unsubscribed_at, now()) ELSE unsubscribed_at END,
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: MirrorNewsletterSubscriptionStatus :one
-- A webhook or a reconcile telling us what Mailchimp now says.
UPDATE newsletter_subscriptions
SET status = sqlc.arg('status'),
    unsubscribe_reason = COALESCE(sqlc.narg('unsubscribe_reason'), unsubscribe_reason),
    unique_email_id = COALESCE(sqlc.narg('unique_email_id'), unique_email_id),
    mailchimp_contact_id = COALESCE(sqlc.narg('mailchimp_contact_id'), mailchimp_contact_id),
    web_id = COALESCE(sqlc.narg('web_id'), web_id),
    subscribed_at = CASE WHEN sqlc.arg('status') = 'subscribed' THEN COALESCE(subscribed_at, sqlc.arg('at')) ELSE subscribed_at END,
    unsubscribed_at = CASE WHEN sqlc.arg('status') IN ('unsubscribed', 'cleaned') THEN COALESCE(unsubscribed_at, sqlc.arg('at')) ELSE unsubscribed_at END,
    last_synced_at = now(), updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: ListNewsletterSubscriptions :many
SELECT s.*, c.email::text AS email, c.lifecycle_stage, a.name AS audience_name
FROM newsletter_subscriptions s
JOIN contacts c ON c.id = s.contact_id
JOIN newsletter_audiences a ON a.id = s.audience_id
WHERE (cardinality(sqlc.arg('statuses')::text[]) = 0 OR s.status = ANY(sqlc.arg('statuses')::text[]))
  AND (cardinality(sqlc.arg('sync_statuses')::text[]) = 0 OR s.sync_status = ANY(sqlc.arg('sync_statuses')::text[]))
  AND (sqlc.narg('audience_id')::uuid IS NULL OR s.audience_id = sqlc.narg('audience_id')::uuid)
  AND (sqlc.narg('q')::text IS NULL OR c.email::text ILIKE '%' || sqlc.narg('q')::text || '%')
ORDER BY s.updated_at DESC, s.id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountNewsletterSubscriptions :one
SELECT count(*) FROM newsletter_subscriptions s
JOIN contacts c ON c.id = s.contact_id
WHERE (cardinality(sqlc.arg('statuses')::text[]) = 0 OR s.status = ANY(sqlc.arg('statuses')::text[]))
  AND (cardinality(sqlc.arg('sync_statuses')::text[]) = 0 OR s.sync_status = ANY(sqlc.arg('sync_statuses')::text[]))
  AND (sqlc.narg('audience_id')::uuid IS NULL OR s.audience_id = sqlc.narg('audience_id')::uuid)
  AND (sqlc.narg('q')::text IS NULL OR c.email::text ILIKE '%' || sqlc.narg('q')::text || '%');

-- name: CountNewsletterSubscriptionsByStatus :many
SELECT status, count(*)::bigint AS total FROM newsletter_subscriptions GROUP BY status;

-- name: ListNewsletterSubscriptionsToReconcile :many
SELECT * FROM newsletter_subscriptions
WHERE sync_status = 'synced' AND status IN ('pending', 'subscribed')
  AND (last_synced_at IS NULL OR last_synced_at < sqlc.arg('before'))
ORDER BY last_synced_at NULLS FIRST
LIMIT sqlc.arg('lim');

-- name: ListNewsletterEligibleContacts :many
-- Contacts who could be pushed now, and those one step short with the reason why.
SELECT c.*, cc.id AS consent_id, cc.source AS consent_source, cc.captured_at AS consent_captured_at
FROM contacts c
LEFT JOIN contact_consents cc ON cc.contact_id = c.id AND cc.revoked_at IS NULL
WHERE c.lifecycle_stage IN ('permission_requested', 'permission_captured', 'newsletter_eligible')
  AND (sqlc.narg('campaign_id')::uuid IS NULL
       OR c.id IN (SELECT contact_id FROM campaign_leads WHERE campaign_id = sqlc.narg('campaign_id')::uuid))
ORDER BY c.stage_changed_at DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountNewsletterEligibleContacts :one
SELECT count(*) FROM contacts c
WHERE c.lifecycle_stage IN ('permission_requested', 'permission_captured', 'newsletter_eligible')
  AND (sqlc.narg('campaign_id')::uuid IS NULL
       OR c.id IN (SELECT contact_id FROM campaign_leads WHERE campaign_id = sqlc.narg('campaign_id')::uuid));
