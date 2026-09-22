-- name: UpsertEmailVerification :one
-- Creates the row the first time an address is verified. The no-op DO UPDATE lets a
-- single statement both insert and return the existing row.
INSERT INTO email_verifications (id, email, domain)
VALUES (sqlc.arg('id'), sqlc.arg('email'), sqlc.arg('domain'))
ON CONFLICT (email) DO UPDATE SET updated_at = email_verifications.updated_at
RETURNING *;

-- name: GetEmailVerification :one
SELECT * FROM email_verifications WHERE id = $1;

-- name: GetEmailVerificationByEmail :one
SELECT * FROM email_verifications WHERE email = $1;

-- name: UpdateVerificationPass1 :one
-- The free stage writes the local pipeline's own breakdown and the weighted result of
-- every free provider in one statement, so a row can never show a free score without
-- the breakdown that explains it.
UPDATE email_verifications
SET pass1_score       = sqlc.arg('pass1_score'),
    pass1_checks      = sqlc.arg('pass1_checks'),
    pass1_hard_fail   = sqlc.narg('pass1_hard_fail'),
    pass1_verified_at = now(),
    free_score        = sqlc.arg('free_score'),
    free_scored_at    = now(),
    provider_results  = sqlc.arg('provider_results'),
    typo_suggestion   = sqlc.narg('typo_suggestion'),
    final_score       = sqlc.arg('final_score'),
    verification_tag  = sqlc.arg('verification_tag'),
    last_error        = NULL,
    updated_at        = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: ClaimThirdPartySend :one
-- The hard check that stops an address reaching a third party twice.
--
-- It is a compare-and-set, not a read followed by a write: two concurrent runs racing
-- over the same address both reach this statement, and exactly one of them updates a
-- row. The loser gets no rows back and must skip. Nothing is billed before this
-- succeeds, so the claim is what authorises the outbound call.
UPDATE email_verifications
SET third_party_sent_at = now(),
    updated_at          = now()
WHERE id = sqlc.arg('id')
  AND third_party_sent_at IS NULL
RETURNING *;

-- name: ReleaseThirdPartySend :execrows
-- Gives the claim back when the call never reached the provider: a rejected API key,
-- a throttle, a transport failure. Our own infrastructure failing must not spend an
-- address's one and only send. The pass2_verified_at guard means a claim that did
-- produce a verdict can never be released, and the database enforces that too.
UPDATE email_verifications
SET third_party_sent_at = NULL,
    updated_at          = now()
WHERE id = sqlc.arg('id')
  AND pass2_verified_at IS NULL;

-- name: UpdateVerificationPass2 :one
-- A billable outcome: deliverable, risky or undeliverable. Written once and never
-- again; third_party_sent_at is already held by the claim that authorised the call.
UPDATE email_verifications
SET pass2_score       = sqlc.arg('pass2_score'),
    pass2_status      = sqlc.arg('pass2_status'),
    pass2_raw         = sqlc.arg('pass2_raw'),
    pass2_source_id   = sqlc.narg('pass2_source_id'),
    pass2_credits     = pass2_credits + sqlc.arg('credits'),
    pass2_verified_at = now(),
    final_score       = sqlc.arg('final_score'),
    verification_tag  = sqlc.arg('verification_tag'),
    last_error        = NULL,
    updated_at        = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: UpdateVerificationPass2Inconclusive :one
-- "unknown" and hard errors keep the free score and store no verdict, so
-- pass2_verified_at stays as it was. Whether the address may be tried again is not
-- decided here but by third_party_sent_at: an answer of "unknown" came from the
-- provider and keeps the lock, while a call that never arrived releases it.
UPDATE email_verifications
SET pass2_status  = sqlc.arg('pass2_status'),
    pass2_raw     = sqlc.narg('pass2_raw'),
    pass2_credits = pass2_credits + sqlc.arg('credits'),
    last_error    = sqlc.narg('last_error'),
    updated_at    = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SetVerificationError :exec
UPDATE email_verifications
SET last_error = sqlc.narg('last_error'), updated_at = now()
WHERE id = sqlc.arg('id');

-- name: ClearTypoSuggestion :exec
UPDATE email_verifications
SET typo_suggestion = NULL, updated_at = now()
WHERE id = $1;

-- name: ListBusinessesForEmail :many
SELECT b.id, b.name
FROM business_emails be
         JOIN businesses b ON b.id = be.business_id
WHERE be.email = $1
ORDER BY b.name, b.id;

-- name: ListBusinessIDsForEmail :many
SELECT DISTINCT business_id FROM business_emails WHERE email = $1;

-- name: DeleteShadowedBusinessEmails :execrows
-- When a business already holds the corrected address, its typo'd row is dropped
-- rather than updated, because (business_id, email) is unique.
DELETE FROM business_emails old
WHERE old.email = sqlc.arg('old_email')
  AND EXISTS (SELECT 1
              FROM business_emails current
              WHERE current.business_id = old.business_id
                AND current.email = sqlc.arg('new_email'));

-- name: RetargetBusinessEmails :execrows
UPDATE business_emails
SET email = sqlc.arg('new_email')
WHERE email = sqlc.arg('old_email');
