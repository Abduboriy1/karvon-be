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
