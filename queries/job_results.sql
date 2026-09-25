-- name: UpsertJobResult :exec
INSERT INTO job_results (job_id, business_id, query_id)
VALUES (sqlc.arg('job_id'), sqlc.arg('business_id'), sqlc.narg('query_id'))
ON CONFLICT (job_id, business_id) DO NOTHING;

-- name: CountJobResults :one
SELECT count(*) FROM job_results WHERE job_id = $1;

-- name: CountJobSitesTotal :one
SELECT count(*)
FROM job_results jr
         JOIN businesses b ON b.id = jr.business_id
WHERE jr.job_id = $1
  AND b.website IS NOT NULL
  AND b.website <> '';

-- name: CopyJobResults :execrows
INSERT INTO job_results (job_id, business_id)
SELECT sqlc.arg('target_job_id')::uuid, src.business_id
FROM job_results src
WHERE src.job_id = sqlc.arg('source_job_id')::uuid
ON CONFLICT (job_id, business_id) DO NOTHING;

-- name: AddJobResults :execrows
-- Attaches hand-picked businesses to a re-crawl job.
INSERT INTO job_results (job_id, business_id)
SELECT sqlc.arg('job_id')::uuid, unnest(sqlc.arg('business_ids')::uuid[])
ON CONFLICT (job_id, business_id) DO NOTHING;

-- name: PickRecrawlSource :one
-- A job row needs a source even when it never searches. Prefer the Maps source that
-- first found one of the businesses, then the oldest Maps source.
SELECT s.id
FROM sources s
WHERE s.role = 'maps'
ORDER BY EXISTS (SELECT 1
                 FROM businesses b
                          JOIN jobs j ON j.id = b.first_job_id
                 WHERE b.id = ANY (sqlc.arg('business_ids')::uuid[])
                   AND j.source_id = s.id) DESC,
         s.created_at
LIMIT 1;
