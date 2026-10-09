-- name: CreateCleanupRun :one
-- The partial unique index refuses a second run while one is queued or running.
-- created_at comes from the service clock: every batch measures the idle window
-- from it, so it must agree with the clock the preview and the count used.
INSERT INTO instantly_cleanup_runs (id, trigger, scope, min_idle_days, include_replied, campaign_ids, max_leads, selected, created_at)
VALUES (sqlc.arg('id'), sqlc.arg('trigger'), sqlc.arg('scope'), sqlc.arg('min_idle_days'), sqlc.arg('include_replied'),
        sqlc.arg('campaign_ids')::uuid[], sqlc.narg('max_leads'), sqlc.arg('selected'), sqlc.arg('created_at'))
RETURNING *;

-- name: GetCleanupRun :one
SELECT * FROM instantly_cleanup_runs WHERE id = $1;

-- name: GetActiveCleanupRun :one
SELECT * FROM instantly_cleanup_runs WHERE status IN ('queued', 'running') ORDER BY created_at DESC LIMIT 1;

-- name: GetLatestCleanupRun :one
SELECT * FROM instantly_cleanup_runs ORDER BY created_at DESC LIMIT 1;

-- name: ListCleanupRuns :many
SELECT * FROM instantly_cleanup_runs ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountCleanupRuns :one
SELECT count(*) FROM instantly_cleanup_runs;

-- name: StartCleanupRun :one
UPDATE instantly_cleanup_runs
SET status = 'running', started_at = COALESCE(started_at, sqlc.arg('at'))
WHERE id = sqlc.arg('id') AND status IN ('queued', 'running')
RETURNING *;

-- name: AddCleanupRunCounts :exec
UPDATE instantly_cleanup_runs
SET removed      = removed + sqlc.arg('removed'),
    already_gone = already_gone + sqlc.arg('already_gone'),
    failed       = failed + sqlc.arg('failed')
WHERE id = sqlc.arg('id');

-- name: FinishCleanupRun :one
UPDATE instantly_cleanup_runs
SET status = sqlc.arg('status'), error = sqlc.narg('error'), finished_at = sqlc.arg('at')
WHERE id = sqlc.arg('id') AND status IN ('queued', 'running')
RETURNING *;
