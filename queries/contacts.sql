-- name: UpsertContact :one
-- Creates the contact the first time an address is seen. The no-op DO UPDATE lets one
-- statement insert and return the existing row; it never overwrites the stage.
INSERT INTO contacts (id, email, domain, first_name, last_name, company, title, phone, website, business_id, source)
VALUES (sqlc.arg('id'), sqlc.arg('email'), sqlc.arg('domain'), sqlc.narg('first_name'), sqlc.narg('last_name'),
        sqlc.narg('company'), sqlc.narg('title'), sqlc.narg('phone'), sqlc.narg('website'),
        sqlc.narg('business_id'), sqlc.arg('source'))
ON CONFLICT (email) DO UPDATE
SET first_name  = COALESCE(contacts.first_name, EXCLUDED.first_name),
    last_name   = COALESCE(contacts.last_name, EXCLUDED.last_name),
    company     = COALESCE(contacts.company, EXCLUDED.company),
    title       = COALESCE(contacts.title, EXCLUDED.title),
    phone       = COALESCE(contacts.phone, EXCLUDED.phone),
    website     = COALESCE(contacts.website, EXCLUDED.website),
    business_id = COALESCE(contacts.business_id, EXCLUDED.business_id),
    updated_at  = now()
RETURNING *;

-- name: GetContact :one
SELECT * FROM contacts WHERE id = $1;

-- name: GetContactByEmail :one
SELECT * FROM contacts WHERE email = $1;

-- name: UpdateContactProfile :one
UPDATE contacts
SET first_name = COALESCE(sqlc.narg('first_name'), first_name),
    last_name  = COALESCE(sqlc.narg('last_name'), last_name),
    company    = COALESCE(sqlc.narg('company'), company),
    title      = COALESCE(sqlc.narg('title'), title),
    phone      = COALESCE(sqlc.narg('phone'), phone),
    website    = COALESCE(sqlc.narg('website'), website),
    attributes = COALESCE(sqlc.narg('attributes'), attributes),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SetContactStage :one
-- The trigger enforces the rules; this statement only moves the pointer.
UPDATE contacts
SET lifecycle_stage = sqlc.arg('stage'), last_event_at = now(), updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SuppressContact :one
UPDATE contacts
SET suppressed_at      = COALESCE(suppressed_at, now()),
    suppression_reason = sqlc.arg('reason'),
    lifecycle_stage    = sqlc.arg('stage'),
    last_event_at      = now(),
    updated_at         = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: LiftContactSuppression :one
UPDATE contacts
SET suppressed_at      = NULL,
    suppression_reason = NULL,
    lifecycle_stage    = sqlc.arg('stage'),
    last_event_at      = now(),
    updated_at         = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: TouchContactEvent :exec
UPDATE contacts SET last_event_at = GREATEST(COALESCE(last_event_at, sqlc.arg('at')), sqlc.arg('at')) WHERE id = sqlc.arg('id');

-- name: CountContactsByStage :many
SELECT lifecycle_stage, count(*)::bigint AS total FROM contacts GROUP BY lifecycle_stage;

-- name: CountCampaignContactsByStage :many
SELECT c.lifecycle_stage, count(*)::bigint AS total
FROM contacts c
WHERE c.id IN (SELECT contact_id FROM campaign_leads WHERE campaign_id = $1)
GROUP BY c.lifecycle_stage;

-- name: CreateContactConsent :one
INSERT INTO contact_consents (id, contact_id, source, captured_at, evidence, captured_by, campaign_lead_id, provider_event_id)
VALUES (sqlc.arg('id'), sqlc.arg('contact_id'), sqlc.arg('source'), sqlc.arg('captured_at'), sqlc.arg('evidence'),
        sqlc.arg('captured_by'), sqlc.narg('campaign_lead_id'), sqlc.narg('provider_event_id'))
RETURNING *;

-- name: GetActiveConsent :one
SELECT * FROM contact_consents WHERE contact_id = $1 AND revoked_at IS NULL;

-- name: GetContactConsent :one
SELECT * FROM contact_consents WHERE id = $1;

-- name: ListContactConsents :many
SELECT * FROM contact_consents WHERE contact_id = $1 ORDER BY created_at DESC;

-- name: RevokeConsent :one
UPDATE contact_consents
SET revoked_at = now(), revoke_reason = sqlc.narg('reason')
WHERE id = sqlc.arg('id') AND revoked_at IS NULL
RETURNING *;

-- name: CreateContactSuppression :one
INSERT INTO contact_suppressions (id, contact_id, reason, source, note, campaign_lead_id, provider_event_id)
VALUES (sqlc.arg('id'), sqlc.arg('contact_id'), sqlc.arg('reason'), sqlc.arg('source'), sqlc.narg('note'),
        sqlc.narg('campaign_lead_id'), sqlc.narg('provider_event_id'))
RETURNING *;

-- name: GetActiveSuppression :one
SELECT * FROM contact_suppressions WHERE contact_id = $1 AND lifted_at IS NULL;

-- name: GetContactSuppression :one
SELECT * FROM contact_suppressions WHERE id = $1;

-- name: ListContactSuppressions :many
SELECT * FROM contact_suppressions WHERE contact_id = $1 ORDER BY created_at DESC;

-- name: LiftSuppression :one
UPDATE contact_suppressions
SET lifted_at = now(), lifted_note = sqlc.narg('note')
WHERE id = sqlc.arg('id') AND lifted_at IS NULL
RETURNING *;
