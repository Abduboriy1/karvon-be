-- name: GetRegistrarSettings :one
SELECT * FROM registrar_settings WHERE id = 1;

-- name: SetRegistrarAccountID :one
UPDATE registrar_settings
SET cloudflare_account_id = sqlc.narg('account_id'),
    updated_at            = now()
WHERE id = 1
RETURNING *;

-- name: CreateDomainPurchase :one
INSERT INTO domain_purchases (id, auto_renew, currency, quoted_total_cents, item_count)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: CreateDomainPurchaseItem :one
INSERT INTO domain_purchase_items (id, purchase_id, position, domain_name, quoted_cost_cents,
                                   cost_cents, renewal_cost_cents)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetDomainPurchase :one
SELECT * FROM domain_purchases WHERE id = $1;

-- name: GetActiveDomainPurchase :one
SELECT * FROM domain_purchases
WHERE status IN ('queued', 'processing')
ORDER BY created_at
LIMIT 1;

-- name: ListDomainPurchases :many
SELECT * FROM domain_purchases
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountDomainPurchases :one
SELECT count(*) FROM domain_purchases;

-- name: ListDomainPurchaseItems :many
SELECT * FROM domain_purchase_items WHERE purchase_id = $1 ORDER BY position;

-- name: ListDomainPurchaseItemsFor :many
SELECT * FROM domain_purchase_items
WHERE purchase_id = ANY(sqlc.arg('purchase_ids')::uuid[])
ORDER BY purchase_id, position;

-- name: StartDomainPurchase :one
UPDATE domain_purchases
SET status     = 'processing',
    started_at = COALESCE(started_at, now()),
    updated_at = now()
WHERE id = $1 AND status IN ('queued', 'processing')
RETURNING *;

-- name: FinishDomainPurchase :one
UPDATE domain_purchases
SET status      = sqlc.arg('status'),
    error       = sqlc.narg('error'),
    finished_at = now(),
    updated_at  = now()
WHERE id = sqlc.arg('id') AND status IN ('queued', 'processing')
RETURNING *;

-- name: SetDomainItemPrice :exec
UPDATE domain_purchase_items
SET cost_cents         = sqlc.narg('cost_cents'),
    renewal_cost_cents = sqlc.narg('renewal_cost_cents'),
    updated_at         = now()
WHERE id = sqlc.arg('id') AND status = 'pending';

-- The row says "registering" before the billable call is made, so a worker that dies
-- mid-call leaves a row that is reconciled with Cloudflare instead of retried blind.
-- name: MarkDomainItemRegistering :one
UPDATE domain_purchase_items
SET status            = 'registering',
    register_attempts = register_attempts + 1,
    attempted_at      = now(),
    updated_at        = now()
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: ResetDomainItemPending :exec
UPDATE domain_purchase_items
SET status = 'pending', updated_at = now()
WHERE id = $1 AND status = 'registering';

-- name: SucceedDomainItem :exec
UPDATE domain_purchase_items
SET status        = 'succeeded',
    registered_at = COALESCE(sqlc.narg('registered_at'), now()),
    expires_at    = sqlc.narg('expires_at'),
    error_code    = NULL,
    error_message = NULL,
    updated_at    = now()
WHERE id = sqlc.arg('id') AND status IN ('pending', 'registering');

-- name: FailDomainItem :exec
UPDATE domain_purchase_items
SET status        = sqlc.arg('status'),
    error_code    = sqlc.narg('error_code'),
    error_message = sqlc.narg('error_message'),
    updated_at    = now()
WHERE id = sqlc.arg('id') AND status IN ('pending', 'registering');
