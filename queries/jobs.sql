-- name: CreateJob :one
INSERT INTO jobs (id, name, source_id, config, stats, status)
VALUES (sqlc.arg('id'), sqlc.arg('name'), sqlc.arg('source_id'), sqlc.arg('config'),
        sqlc.arg('stats'), 'queued')
RETURNING *;

-- name: GetJob :one
SELECT j.*, s.name AS source_name, s.kind AS source_kind
FROM jobs j
         JOIN sources s ON s.id = j.source_id
WHERE j.id = $1;

-- name: GetJobStatus :one
SELECT status FROM jobs WHERE id = $1;


-- name: MarkJobRunning :exec
UPDATE jobs
SET status     = 'running',
    started_at = COALESCE(started_at, now())
WHERE id = $1
  AND status = 'queued';

-- name: MarkJobTerminal :exec
UPDATE jobs
SET status      = sqlc.arg('status'),
    error       = sqlc.narg('error'),
    finished_at = now()
WHERE id = sqlc.arg('id')
  AND status IN ('queued', 'running');

-- name: CancelJob :one
UPDATE jobs
SET status      = 'cancelled',
    finished_at = now()
WHERE id = sqlc.arg('id')
  AND status IN ('queued', 'running')
RETURNING *;

-- name: DeleteJob :execrows
DELETE FROM jobs WHERE id = $1;

-- name: SetJobStats :exec
UPDATE jobs SET stats = sqlc.arg('stats') WHERE id = sqlc.arg('id');

-- name: BumpJobStats :exec
UPDATE jobs
SET stats = jsonb_set(stats, ARRAY[sqlc.arg('key')::text],
                      to_jsonb(COALESCE((stats ->> sqlc.arg('key')::text)::bigint, 0)
                                   + sqlc.arg('delta')::bigint))
WHERE id = sqlc.arg('id');

-- name: GetJobStats :one
SELECT stats FROM jobs WHERE id = $1;

-- name: RecomputeJobStats :one
WITH q AS (SELECT count(*)                                             AS total,
                  count(*) FILTER (WHERE jq.status IN ('done', 'failed', 'cancelled')) AS done,
                  count(*) FILTER (WHERE jq.status = 'failed')         AS failed,
                  COALESCE(sum(jq.listings_found), 0)                  AS listings
           FROM job_queries jq
           WHERE jq.job_id = sqlc.arg('jid')),
     b AS (SELECT count(*)                                                       AS sites_total,
                  count(*) FILTER (WHERE bu.last_crawled_at IS NOT NULL)         AS sites_crawled
           FROM job_results jr
                    JOIN businesses bu ON bu.id = jr.business_id
           WHERE jr.job_id = sqlc.arg('jid')
             AND bu.website IS NOT NULL
             AND bu.website <> ''),
     e AS (SELECT count(*) AS emails
           FROM job_results jr
                    JOIN business_emails be ON be.business_id = jr.business_id
           WHERE jr.job_id = sqlc.arg('jid'))
SELECT q.total::bigint         AS queries_total,
       q.done::bigint          AS queries_done,
       q.failed::bigint        AS queries_failed,
       q.listings::bigint      AS listings_found,
       b.sites_total::bigint   AS sites_total,
       b.sites_crawled::bigint AS sites_crawled,
       e.emails::bigint        AS emails_found
FROM q, b, e;
