-- name: InsertBusinessEmail :one
INSERT INTO business_emails (id, business_id, email, source, page_url, is_primary)
VALUES (sqlc.arg('id'), sqlc.arg('business_id'), sqlc.arg('email'), sqlc.arg('source'),
        sqlc.narg('page_url'), false)
ON CONFLICT (business_id, email) DO UPDATE
    SET page_url = COALESCE(business_emails.page_url, EXCLUDED.page_url)
RETURNING *;

-- name: ListBusinessEmails :many
SELECT * FROM business_emails
WHERE business_id = $1
ORDER BY is_primary DESC, found_at, email;

-- name: ListBusinessEmailsWithVerification :many
-- The detail view needs each address's verification alongside it; the LEFT JOIN keeps
-- addresses that have never been through Pass 1.
SELECT be.*,
       ev.id               AS verification_id,
       ev.final_score      AS verification_score,
       ev.verification_tag AS verification_tag,
       ev.typo_suggestion  AS typo_suggestion,
       ev.pass1_score      AS pass1_score,
       ev.pass2_status     AS pass2_status,
       ev.pass1_verified_at AS pass1_verified_at,
       ev.pass2_verified_at AS pass2_verified_at
FROM business_emails be
         LEFT JOIN email_verifications ev ON ev.email = be.email
WHERE be.business_id = $1
ORDER BY be.is_primary DESC, be.found_at, be.email;

-- name: ClearPrimaryEmail :exec
UPDATE business_emails SET is_primary = false WHERE business_id = $1 AND is_primary;

-- name: SetPrimaryEmail :exec
UPDATE business_emails SET is_primary = true WHERE id = $1;
