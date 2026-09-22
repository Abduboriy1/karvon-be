-- name: CreateJobQuery :one
INSERT INTO job_queries (id, job_id, term, terms, city, state)
VALUES (sqlc.arg('id'), sqlc.arg('job_id'), sqlc.arg('term'), sqlc.arg('terms'),
        sqlc.arg('city'), sqlc.arg('state'))
ON CONFLICT (job_id, term, city, state) DO UPDATE SET terms = EXCLUDED.terms
RETURNING *;

-- name: ListJobQueries :many
SELECT * FROM job_queries WHERE job_id = $1 ORDER BY created_at, id;

-- name: GetJobQuery :one
SELECT * FROM job_queries WHERE id = $1;

-- name: MarkJobQueryRunning :exec
UPDATE job_queries
SET status = 'running', started_at = now()
WHERE id = $1;

-- name: MarkJobQueryDone :exec
UPDATE job_queries
SET status         = 'done',
    run_state      = 'finished',
    listings_found = sqlc.arg('listings_found'),
    provider_run_id = COALESCE(sqlc.narg('provider_run_id'), provider_run_id),
    cost_cents     = sqlc.arg('cost_cents'),
    finished_at    = now()
WHERE id = sqlc.arg('id');

-- name: MarkJobQueryFailed :exec
UPDATE job_queries
SET status = sqlc.arg('status'), run_state = 'finished', error = sqlc.narg('error'),
    finished_at = now()
WHERE id = sqlc.arg('id');

-- name: CountPendingJobQueries :one
SELECT count(*) FROM job_queries
WHERE job_id = $1 AND status NOT IN ('done', 'failed', 'cancelled');

-- name: CancelPendingJobQueries :exec
UPDATE job_queries
SET status = 'cancelled', run_state = 'finished', finished_at = now()
WHERE job_id = $1 AND status IN ('queued', 'running');

-- Asynchronous provider runs.
--
-- The claim below is what stops a retry from paying for a second run: a row leaves
-- 'none' exactly once, and only a row in 'none' may start one.

-- name: LockProviderRunSlots :exec
SELECT pg_advisory_xact_lock(sqlc.arg('key')::bigint);

-- name: CountActiveProviderRuns :one
SELECT count(*)
FROM job_queries jq
         JOIN jobs j ON j.id = jq.job_id
WHERE j.source_id = sqlc.arg('source_id')
  AND jq.run_state IN ('starting', 'polling')
  -- A query that reached a terminal state holds no slot even if its run state was
  -- never tidied up, so a lost queue entry cannot occupy the account forever.
  AND jq.status NOT IN ('done', 'failed', 'cancelled');

-- name: ClaimProviderRunSlot :execrows
UPDATE job_queries
SET status = 'running', run_state = 'starting', started_at = now()
WHERE id = sqlc.arg('id') AND run_state = 'none';

-- name: ReleaseProviderRunSlot :exec
UPDATE job_queries
SET run_state = 'none'
WHERE id = sqlc.arg('id') AND run_state = 'starting' AND provider_run_id IS NULL;

-- name: SaveProviderRun :exec
UPDATE job_queries
SET provider_run_id = sqlc.arg('provider_run_id'),
    dataset_id      = sqlc.narg('dataset_id'),
    run_status      = sqlc.narg('run_status'),
    run_state       = 'polling'
WHERE id = sqlc.arg('id');

-- name: SetProviderRunStatus :exec
UPDATE job_queries
SET run_status = sqlc.narg('run_status'),
    run_state  = sqlc.arg('run_state')
WHERE id = sqlc.arg('id');

-- name: MarkProviderRunResurrected :exec
UPDATE job_queries
SET resurrected = true, run_state = 'polling'
WHERE id = sqlc.arg('id');

-- name: AdvanceProviderRunIngest :exec
UPDATE job_queries
SET ingested_offset = sqlc.arg('ingested_offset'),
    listings_found  = sqlc.arg('listings_found')
WHERE id = sqlc.arg('id');

-- name: ListProviderRunIDsForSource :many
SELECT DISTINCT jq.provider_run_id
FROM job_queries jq
         JOIN jobs j ON j.id = jq.job_id
WHERE j.source_id = sqlc.arg('source_id')
  AND jq.provider_run_id IS NOT NULL;
