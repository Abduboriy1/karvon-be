-- name: CreateEmailComponent :one
INSERT INTO email_components (id, type, name, body, status, tags, language, ai_generation_id, placeholders)
VALUES (sqlc.arg('id'), sqlc.arg('type'), sqlc.arg('name'), sqlc.arg('body'), sqlc.arg('status'), sqlc.arg('tags'),
        sqlc.arg('language'), sqlc.narg('ai_generation_id'), sqlc.arg('placeholders'))
RETURNING *;

-- name: GetEmailComponent :one
SELECT * FROM email_components WHERE id = $1;

-- name: ListEmailComponentsByIDs :many
SELECT * FROM email_components WHERE id = ANY(sqlc.arg('ids')::uuid[]);

-- name: ListEmailComponents :many
SELECT * FROM email_components
WHERE (cardinality(sqlc.arg('types')::text[]) = 0 OR type = ANY(sqlc.arg('types')::text[]))
  AND (cardinality(sqlc.arg('statuses')::text[]) = 0 OR status = ANY(sqlc.arg('statuses')::text[]))
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY(tags))
  AND (sqlc.narg('q')::text IS NULL OR name ILIKE '%' || sqlc.narg('q')::text || '%' OR body ILIKE '%' || sqlc.narg('q')::text || '%')
ORDER BY type, created_at DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountEmailComponents :one
SELECT count(*) FROM email_components
WHERE (cardinality(sqlc.arg('types')::text[]) = 0 OR type = ANY(sqlc.arg('types')::text[]))
  AND (cardinality(sqlc.arg('statuses')::text[]) = 0 OR status = ANY(sqlc.arg('statuses')::text[]))
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY(tags))
  AND (sqlc.narg('q')::text IS NULL OR name ILIKE '%' || sqlc.narg('q')::text || '%' OR body ILIKE '%' || sqlc.narg('q')::text || '%');

-- name: UpdateEmailComponent :one
UPDATE email_components
SET name         = COALESCE(sqlc.narg('name'), name),
    body         = COALESCE(sqlc.narg('body'), body),
    tags         = COALESCE(sqlc.narg('tags'), tags),
    language     = COALESCE(sqlc.narg('language'), language),
    placeholders = COALESCE(sqlc.narg('placeholders'), placeholders),
    updated_at   = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SetEmailComponentStatus :one
UPDATE email_components
SET status = sqlc.arg('status'),
    archived_at = CASE WHEN sqlc.arg('status') = 'archived' THEN now() ELSE NULL END,
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: CountLockedAssignmentsForComponent :one
SELECT count(*) FROM variant_assignments WHERE locked_at IS NOT NULL AND sqlc.arg('component_id')::uuid = ANY(component_ids);

-- name: ListVariantsUsingComponent :many
SELECT v.* FROM email_variants v
JOIN email_variant_components vc ON vc.variant_id = v.id
WHERE vc.component_id = $1
ORDER BY v.created_at DESC;

-- name: CreateEmailVariant :one
INSERT INTO email_variants (id, name, step, status, subject_template, body_template, component_ids, ai_generation_id, notes)
VALUES (sqlc.arg('id'), sqlc.arg('name'), sqlc.arg('step'), sqlc.arg('status'), sqlc.arg('subject_template'),
        sqlc.arg('body_template'), sqlc.arg('component_ids'), sqlc.narg('ai_generation_id'), sqlc.narg('notes'))
RETURNING *;

-- name: GetEmailVariant :one
SELECT * FROM email_variants WHERE id = $1;

-- name: ListEmailVariantsByIDs :many
SELECT * FROM email_variants WHERE id = ANY(sqlc.arg('ids')::uuid[]);

-- name: ListEmailVariants :many
SELECT * FROM email_variants
WHERE (cardinality(sqlc.arg('statuses')::text[]) = 0 OR status = ANY(sqlc.arg('statuses')::text[]))
  AND (sqlc.narg('step')::int IS NULL OR step = sqlc.narg('step')::int)
  AND (sqlc.narg('q')::text IS NULL OR name ILIKE '%' || sqlc.narg('q')::text || '%')
ORDER BY created_at DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountEmailVariants :one
SELECT count(*) FROM email_variants
WHERE (cardinality(sqlc.arg('statuses')::text[]) = 0 OR status = ANY(sqlc.arg('statuses')::text[]))
  AND (sqlc.narg('step')::int IS NULL OR step = sqlc.narg('step')::int)
  AND (sqlc.narg('q')::text IS NULL OR name ILIKE '%' || sqlc.narg('q')::text || '%');

-- name: UpdateEmailVariant :one
UPDATE email_variants
SET name             = COALESCE(sqlc.narg('name'), name),
    step             = COALESCE(sqlc.narg('step'), step),
    subject_template = COALESCE(sqlc.narg('subject_template'), subject_template),
    body_template    = COALESCE(sqlc.narg('body_template'), body_template),
    component_ids    = COALESCE(sqlc.narg('component_ids'), component_ids),
    notes            = COALESCE(sqlc.narg('notes'), notes),
    updated_at       = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SetEmailVariantStatus :one
UPDATE email_variants
SET status = sqlc.arg('status'),
    archived_at = CASE WHEN sqlc.arg('status') = 'archived' THEN now() ELSE NULL END,
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: ReplaceVariantComponents :exec
DELETE FROM email_variant_components WHERE variant_id = $1;

-- name: AddVariantComponent :exec
INSERT INTO email_variant_components (variant_id, component_id, position, slot) VALUES ($1, $2, $3, $4);

-- name: ListVariantComponents :many
SELECT vc.position, vc.slot, c.* FROM email_variant_components vc
JOIN email_components c ON c.id = vc.component_id
WHERE vc.variant_id = $1
ORDER BY vc.position;

-- name: CountLockedAssignmentsForVariant :one
SELECT count(*) FROM variant_assignments WHERE variant_id = $1 AND locked_at IS NOT NULL;
