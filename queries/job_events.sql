-- name: InsertJobEvent :one
INSERT INTO job_events (job_id, type, data)
VALUES (sqlc.arg('job_id'), sqlc.arg('type'), sqlc.arg('data'))
RETURNING id, ts;

-- name: ListJobEventsAfter :many
SELECT id, job_id, ts, type, data
FROM job_events
WHERE job_id = sqlc.arg('job_id') AND id > sqlc.arg('after_id')
ORDER BY id
LIMIT sqlc.arg('lim');


-- name: PruneJobEvents :execrows
DELETE FROM job_events WHERE ts < now() - make_interval(days => sqlc.arg('max_age_days')::int);

-- name: ListRecentJobEvents :many
SELECT id, job_id, ts, type, data
FROM job_events
WHERE job_id = sqlc.arg('job_id')
ORDER BY id DESC
LIMIT sqlc.arg('lim');
