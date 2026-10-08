-- name: CreateBrandScanDismissal :one
INSERT INTO brand_scan_dismissals (id, group_by, key, note)
VALUES (sqlc.arg('id'), sqlc.arg('group_by'), sqlc.arg('key'), sqlc.narg('note'))
RETURNING *;

-- name: DeleteBrandScanDismissal :execrows
DELETE FROM brand_scan_dismissals WHERE id = $1;

-- name: ListBrandScanDismissals :many
SELECT * FROM brand_scan_dismissals
WHERE (sqlc.narg('group_by')::text IS NULL OR group_by = sqlc.narg('group_by'))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountBrandScanDismissals :one
SELECT count(*) FROM brand_scan_dismissals
WHERE (sqlc.narg('group_by')::text IS NULL OR group_by = sqlc.narg('group_by'));
