-- name: GetVerificationDomain :one
SELECT * FROM verification_domains WHERE domain = $1;

-- name: UpsertVerificationDNS :one
INSERT INTO verification_domains (domain, has_mx, has_a, has_spf, has_dmarc, mx_hosts,
                                  dns_checked_at)
VALUES (sqlc.arg('domain'), sqlc.arg('has_mx'), sqlc.arg('has_a'), sqlc.arg('has_spf'),
        sqlc.arg('has_dmarc'), sqlc.arg('mx_hosts'), now())
ON CONFLICT (domain) DO UPDATE
    SET has_mx         = EXCLUDED.has_mx,
        has_a          = EXCLUDED.has_a,
        has_spf        = EXCLUDED.has_spf,
        has_dmarc      = EXCLUDED.has_dmarc,
        mx_hosts       = EXCLUDED.mx_hosts,
        dns_checked_at = now()
RETURNING *;

-- name: UpsertVerificationRDAP :exec
INSERT INTO verification_domains (domain, registered_at, rdap_checked_at)
VALUES (sqlc.arg('domain'), sqlc.narg('registered_at'), now())
ON CONFLICT (domain) DO UPDATE
    SET registered_at   = EXCLUDED.registered_at,
        rdap_checked_at = now();
