-- name: UpsertBusinessByPlaceID :one
INSERT INTO businesses (id, place_id, name, category, address, city, state, zip, phone, website,
                        domain, rating, reviews, lat, lng, raw, first_job_id)
VALUES (sqlc.arg('id'), sqlc.arg('place_id'), sqlc.arg('name'), sqlc.narg('category'),
        sqlc.narg('address'), sqlc.narg('city'), sqlc.narg('state'), sqlc.narg('zip'),
        sqlc.narg('phone'), sqlc.narg('website'), sqlc.narg('domain'), sqlc.narg('rating'),
        sqlc.narg('reviews'), sqlc.narg('lat'), sqlc.narg('lng'), sqlc.arg('raw'),
        sqlc.arg('first_job_id'))
ON CONFLICT (place_id) DO UPDATE
    SET name       = EXCLUDED.name,
        category   = COALESCE(EXCLUDED.category, businesses.category),
        address    = COALESCE(EXCLUDED.address, businesses.address),
        city       = COALESCE(EXCLUDED.city, businesses.city),
        state      = COALESCE(EXCLUDED.state, businesses.state),
        zip        = COALESCE(EXCLUDED.zip, businesses.zip),
        phone      = COALESCE(EXCLUDED.phone, businesses.phone),
        website    = COALESCE(EXCLUDED.website, businesses.website),
        domain     = COALESCE(EXCLUDED.domain, businesses.domain),
        rating     = COALESCE(EXCLUDED.rating, businesses.rating),
        reviews    = COALESCE(EXCLUDED.reviews, businesses.reviews),
        lat        = COALESCE(EXCLUDED.lat, businesses.lat),
        lng        = COALESCE(EXCLUDED.lng, businesses.lng),
        raw        = COALESCE(EXCLUDED.raw, businesses.raw),
        updated_at = now()
RETURNING *;

-- name: InsertBusiness :one
INSERT INTO businesses (id, place_id, name, category, address, city, state, zip, phone, website,
                        domain, rating, reviews, lat, lng, raw, first_job_id)
VALUES (sqlc.arg('id'), sqlc.narg('place_id'), sqlc.arg('name'), sqlc.narg('category'),
        sqlc.narg('address'), sqlc.narg('city'), sqlc.narg('state'), sqlc.narg('zip'),
        sqlc.narg('phone'), sqlc.narg('website'), sqlc.narg('domain'), sqlc.narg('rating'),
        sqlc.narg('reviews'), sqlc.narg('lat'), sqlc.narg('lng'), sqlc.arg('raw'),
        sqlc.arg('first_job_id'))
RETURNING *;

-- name: UpdateBusinessFromListing :one
UPDATE businesses
SET name       = sqlc.arg('name'),
    category   = COALESCE(sqlc.narg('category'), category),
    address    = COALESCE(sqlc.narg('address'), address),
    city       = COALESCE(sqlc.narg('city'), city),
    state      = COALESCE(sqlc.narg('state'), state),
    zip        = COALESCE(sqlc.narg('zip'), zip),
    phone      = COALESCE(sqlc.narg('phone'), phone),
    website    = COALESCE(sqlc.narg('website'), website),
    domain     = COALESCE(sqlc.narg('domain'), domain),
    rating     = COALESCE(sqlc.narg('rating'), rating),
    reviews    = COALESCE(sqlc.narg('reviews'), reviews),
    lat        = COALESCE(sqlc.narg('lat'), lat),
    lng        = COALESCE(sqlc.narg('lng'), lng),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: FindBusinessByDomain :one
SELECT * FROM businesses
WHERE domain = sqlc.arg('domain') AND domain IS NOT NULL
ORDER BY created_at
LIMIT 1;

-- name: FindBusinessByPhoneZip :one
SELECT * FROM businesses
WHERE phone = sqlc.arg('phone') AND zip = sqlc.arg('zip')
ORDER BY created_at
LIMIT 1;

-- name: GetBusiness :one
SELECT b.*,
       (SELECT j.name FROM jobs j WHERE j.id = b.first_job_id)::text AS first_job_name,
       COALESCE((SELECT be.email FROM business_emails be
                 WHERE be.business_id = b.id
                 ORDER BY be.is_primary DESC, be.found_at, be.email LIMIT 1), '')::text AS primary_email,
       COALESCE((SELECT be.source FROM business_emails be
                 WHERE be.business_id = b.id
                 ORDER BY be.is_primary DESC, be.found_at, be.email LIMIT 1), '')::text AS primary_email_source,
       (SELECT count(*) FROM business_emails be WHERE be.business_id = b.id)::bigint AS emails_count
FROM businesses b
WHERE b.id = $1;

-- name: UpdateBusinessFlags :one
UPDATE businesses
SET suppressed = COALESCE(sqlc.narg('suppressed'), suppressed),
    notes      = CASE WHEN sqlc.arg('set_notes')::boolean THEN sqlc.narg('notes') ELSE notes END,
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING id;

-- name: BulkSetSuppressed :execrows
UPDATE businesses
SET suppressed = sqlc.arg('suppressed'), updated_at = now()
WHERE id = ANY (sqlc.arg('ids')::uuid[])
  AND suppressed IS DISTINCT FROM sqlc.arg('suppressed');

-- name: MarkBusinessCrawled :exec
UPDATE businesses SET last_crawled_at = now(), updated_at = now() WHERE id = $1;

-- name: ListJobCrawlTargets :many
-- The job's businesses with a website that still lack what the crawl looks for.
SELECT b.id, b.website, b.domain
FROM job_results jr
         JOIN businesses b ON b.id = jr.business_id
WHERE jr.job_id = sqlc.arg('jid')
  AND b.website IS NOT NULL
  AND b.website <> ''
  AND ((sqlc.arg('want_emails')::bool
        AND NOT EXISTS (SELECT 1 FROM business_emails be WHERE be.business_id = b.id))
    OR (sqlc.arg('want_socials')::bool
        AND NOT EXISTS (SELECT 1 FROM business_socials bs WHERE bs.business_id = b.id)))
ORDER BY b.id;

-- name: ListJobSocialTargets :many
-- The job's businesses with a profile on one network, and that profile (the first one
-- found when the site links to several), optionally only those still without an email.
SELECT DISTINCT ON (b.id) b.id, bs.url
FROM job_results jr
         JOIN businesses b ON b.id = jr.business_id
         JOIN business_socials bs ON bs.business_id = b.id AND bs.network = sqlc.arg('network')
WHERE jr.job_id = sqlc.arg('jid')
  AND (NOT sqlc.arg('missing_email_only')::bool
    OR NOT EXISTS (SELECT 1 FROM business_emails be WHERE be.business_id = b.id))
ORDER BY b.id, bs.found_at, bs.url;

-- name: FillBusinessPhone :execrows
-- Sets the phone number of a business that has none; a number from the Maps provider
-- is never overwritten.
UPDATE businesses
SET phone = sqlc.arg('phone'), updated_at = now()
WHERE id = sqlc.arg('id')
  AND (phone IS NULL OR phone = '');

-- name: ListFreshCrawledSiblings :many
-- Candidates for a sibling: another listing of the same website, not merely the same
-- domain. A city or a franchise often hosts one page per location under a single
-- domain, and each page carries its own address. This matches on the path only
-- (ignoring scheme, "www.", the query string and trailing slashes, the way
-- business.WebsitePathKey computes the argument); the caller compares the query
-- string, which often names the page ("profile.php?id=…", "detail.aspx?s=…").
SELECT b.id, b.website
FROM businesses b
WHERE b.domain = sqlc.arg('domain')
  AND lower(regexp_replace(regexp_replace(b.website, '[?#].*$', ''), '^https?://(www\.)?|/+$', '', 'gi'))
      = sqlc.arg('path_key')::text
  AND b.id <> sqlc.arg('exclude_id')
  AND b.last_crawled_at IS NOT NULL
  AND b.last_crawled_at > now() - make_interval(days => sqlc.arg('max_age_days')::int)
  AND EXISTS (SELECT 1 FROM business_emails be WHERE be.business_id = b.id)
ORDER BY b.last_crawled_at DESC
LIMIT 200;
