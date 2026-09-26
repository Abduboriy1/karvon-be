-- name: ListScrapeCategories :many
-- Defaults first, in seed order, then the user's own categories by name.
SELECT * FROM scrape_categories ORDER BY is_default DESC, created_at, lower(name);

-- name: GetScrapeCategory :one
SELECT * FROM scrape_categories WHERE id = $1;

-- name: CreateScrapeCategory :one
INSERT INTO scrape_categories (id, name, terms)
VALUES (sqlc.arg('id'), sqlc.arg('name'), sqlc.arg('terms')::text[])
RETURNING *;

-- name: UpdateScrapeCategory :one
UPDATE scrape_categories
SET name       = COALESCE(sqlc.narg('name'), name),
    terms      = COALESCE(sqlc.narg('terms')::text[], terms),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: DeleteScrapeCategory :execrows
DELETE FROM scrape_categories WHERE id = $1 AND NOT is_default;
