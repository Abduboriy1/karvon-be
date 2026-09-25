-- name: InsertBusinessSocial :execrows
-- A profile already recorded for the business keeps its first page_url.
INSERT INTO business_socials (id, business_id, network, handle, url, page_url)
VALUES (sqlc.arg('id'), sqlc.arg('business_id'), sqlc.arg('network'), sqlc.arg('handle'),
        sqlc.arg('url'), sqlc.narg('page_url'))
ON CONFLICT (business_id, url) DO NOTHING;

-- name: ListBusinessSocials :many
SELECT * FROM business_socials
WHERE business_id = $1
ORDER BY network, found_at, url;

-- name: ListSocialsForBusinesses :many
-- One round trip for a whole page of the business list.
SELECT * FROM business_socials
WHERE business_id = ANY (sqlc.arg('business_ids')::uuid[])
ORDER BY business_id, network, found_at, url;
