-- name: ScraperCounters :one
SELECT (SELECT count(*) FROM businesses WHERE NOT suppressed)::bigint            AS businesses,
       (SELECT count(DISTINCT business_id) FROM business_emails)::bigint         AS with_email,
       (SELECT count(*) FROM business_emails)::bigint                            AS emails_total,
       (SELECT count(*) FROM jobs)::bigint                                       AS jobs_total;

-- name: EmailsPerJob :many
SELECT j.id, j.name, COALESCE(e.emails, 0)::bigint AS emails
FROM (SELECT id, name, created_at FROM jobs ORDER BY created_at DESC LIMIT sqlc.arg('lim')) j
         LEFT JOIN LATERAL (
    SELECT count(*) AS emails
    FROM job_results jr
             JOIN business_emails be ON be.business_id = jr.business_id
    WHERE jr.job_id = j.id
    ) e ON true
ORDER BY j.created_at;
