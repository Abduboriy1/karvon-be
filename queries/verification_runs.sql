-- name: CreateVerificationRun :one
INSERT INTO verification_runs (id, pass, filter, est_cost_cents, source_id, auto)
VALUES (sqlc.arg('id'), sqlc.arg('pass'), sqlc.arg('filter'), sqlc.arg('est_cost_cents'),
        sqlc.narg('source_id'), sqlc.arg('auto'))
RETURNING *;

-- name: GetVerificationRun :one
SELECT * FROM verification_runs WHERE id = $1;

-- name: MarkVerificationRunRunning :exec
UPDATE verification_runs
SET status = 'running', started_at = COALESCE(started_at, now())
WHERE id = $1 AND status = 'queued';

-- name: SetVerificationRunTotal :exec
UPDATE verification_runs SET total = sqlc.arg('total') WHERE id = sqlc.arg('id');

-- name: MarkVerificationRunTerminal :exec
UPDATE verification_runs
SET status = sqlc.arg('status'), error = sqlc.narg('error'), finished_at = now()
WHERE id = sqlc.arg('id') AND status IN ('queued', 'running');

-- name: CancelVerificationRun :one
UPDATE verification_runs
SET status = 'cancelled', finished_at = now()
WHERE id = $1 AND status IN ('queued', 'running')
RETURNING *;

-- name: RecomputeVerificationRunStats :one
UPDATE verification_runs r
SET done         = counted.done,
    failed       = counted.failed,
    skipped      = counted.skipped,
    credits_used = counted.credits
FROM (SELECT count(*) FILTER (WHERE status = 'done')::int    AS done,
             count(*) FILTER (WHERE status = 'failed')::int  AS failed,
             count(*) FILTER (WHERE status = 'skipped')::int AS skipped,
             COALESCE(sum(credits), 0)::int                  AS credits
      FROM verification_run_items
      WHERE run_id = sqlc.arg('id')) counted
WHERE r.id = sqlc.arg('id')
RETURNING r.*;

-- name: ListVerificationRuns :many
SELECT * FROM verification_runs
WHERE (sqlc.narg('pass')::text IS NULL OR pass = sqlc.narg('pass')::text)
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountVerificationRuns :one
SELECT count(*) FROM verification_runs
WHERE (sqlc.narg('pass')::text IS NULL OR pass = sqlc.narg('pass')::text)
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text);

-- name: CountActiveVerificationRuns :one
SELECT count(*) FROM verification_runs WHERE status IN ('queued', 'running');

-- name: CountRecentActiveRunsForPass :one
-- What the auto sweep waits on. Only runs created inside the window count, so a run
-- that wedged mid-flight cannot switch automatic verification off for good.
SELECT count(*) FROM verification_runs
WHERE pass = sqlc.arg('pass')
  AND status IN ('queued', 'running')
  AND created_at > sqlc.arg('since');

-- name: LastFinishedVerificationRun :one
SELECT * FROM verification_runs
WHERE pass = sqlc.arg('pass') AND finished_at IS NOT NULL
ORDER BY finished_at DESC
LIMIT 1;

-- name: InsertVerificationRunItems :execrows
INSERT INTO verification_run_items (run_id, verification_id)
SELECT sqlc.arg('run_id')::uuid, unnest(sqlc.arg('verification_ids')::uuid[])
ON CONFLICT (run_id, verification_id) DO NOTHING;

-- name: ListQueuedVerificationRunItems :many
SELECT verification_id FROM verification_run_items
WHERE run_id = $1 AND status = 'queued'
ORDER BY verification_id;

-- name: MarkVerificationRunItem :exec
UPDATE verification_run_items
SET status      = sqlc.arg('status'),
    error       = sqlc.narg('error'),
    credits     = sqlc.arg('credits'),
    finished_at = now()
WHERE run_id = sqlc.arg('run_id') AND verification_id = sqlc.arg('verification_id');

-- name: CountPendingVerificationRunItems :one
SELECT count(*) FROM verification_run_items WHERE run_id = $1 AND status = 'queued';

-- name: CancelPendingVerificationRunItems :exec
UPDATE verification_run_items
SET status = 'skipped', error = 'run cancelled', finished_at = now()
WHERE run_id = $1 AND status = 'queued';
