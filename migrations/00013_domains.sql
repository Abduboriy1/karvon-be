-- +goose Up
-- Domains: search, buy and manage domain names through the Cloudflare Registrar API.
--
-- Cloudflare is the registrar and the source of truth for which domains the account
-- owns; nothing here mirrors that list. What is stored is the one thing Cloudflare
-- cannot tell us afterwards: what was asked for, at what price, and how each
-- registration ended. A registration is billed the moment it succeeds and is not
-- refundable, so every step of a purchase is written down before the call that
-- depends on it.
--
-- Two limits are enforced here rather than trusted to the API layer:
--   * a purchase holds at most ten domains (domain_purchase_items.position is 0-9
--     and unique within its purchase), and
--   * only one purchase is ever queued or processing (a partial unique index), so
--     ten is also the most that can be in flight at once.
--
-- Every enum is text plus a CHECK constraint mirrored by Go constants
-- (internal/registrar/registrar.go).

-- The API token reuses the sources table: one encrypted-key path, one connection test.
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_role_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_role_check
    CHECK (role IN ('maps', 'verifier', 'outreach', 'newsletter', 'registrar'));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_kind_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_kind_check
    CHECK (kind IN ('apify', 'outscraper', 'emailable', 'instantly', 'mailchimp', 'cloudflare'));
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO sources (id, kind, name, role, cost_per_1k_cents, enabled)
VALUES ('0192f000-0000-7000-8000-000000000006', 'cloudflare', 'Cloudflare · Domains', 'registrar', 0, false)
ON CONFLICT (kind) DO NOTHING;
-- +goose StatementEnd

-- The part of the connection that is not a secret: which Cloudflare account the
-- token acts on.
-- +goose StatementBegin
CREATE TABLE registrar_settings (
    id                    smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    cloudflare_account_id text CHECK (cloudflare_account_id ~ '^[0-9a-f]{32}$'),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO registrar_settings (id) VALUES (1) ON CONFLICT DO NOTHING;
-- +goose StatementEnd

-- One confirmed checkout of up to ten domains.
-- +goose StatementBegin
CREATE TABLE domain_purchases (
    id                 uuid PRIMARY KEY,
    status             text NOT NULL DEFAULT 'queued'
                       CHECK (status IN ('queued', 'processing', 'succeeded', 'partial', 'failed')),
    auto_renew         boolean NOT NULL DEFAULT false,
    currency           text NOT NULL DEFAULT 'USD',
    -- What the operator confirmed: the sum of the per-domain prices they were shown.
    quoted_total_cents bigint NOT NULL CHECK (quoted_total_cents >= 0),
    item_count         smallint NOT NULL CHECK (item_count BETWEEN 1 AND 10),
    error              text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    started_at         timestamptz,
    finished_at        timestamptz,
    updated_at         timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX domain_purchases_created_at_idx ON domain_purchases (created_at DESC);
-- +goose StatementEnd
-- At most one purchase in flight, across every API replica.
-- +goose StatementBegin
CREATE UNIQUE INDEX domain_purchases_one_active_idx ON domain_purchases ((true))
    WHERE status IN ('queued', 'processing');
-- +goose StatementEnd

-- One domain inside a purchase.
--
--   pending          not yet sent to Cloudflare
--   registering      the registration call was made (or may have been); the outcome
--                    is asked of Cloudflare, never guessed
--   succeeded        registered and billed
--   failed           not registered: unavailable, price changed, rejected
--   action_required  Cloudflare needs a person to finish it in the dashboard
-- +goose StatementBegin
CREATE TABLE domain_purchase_items (
    id                 uuid PRIMARY KEY,
    purchase_id        uuid NOT NULL REFERENCES domain_purchases (id) ON DELETE CASCADE,
    position           smallint NOT NULL CHECK (position BETWEEN 0 AND 9),
    domain_name        text NOT NULL CHECK (length(domain_name) BETWEEN 3 AND 253),
    status             text NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending', 'registering', 'succeeded', 'failed', 'action_required')),
    -- The registration price the operator confirmed, and the one Cloudflare quoted
    -- immediately before registering. A registration never goes ahead above the
    -- confirmed price.
    quoted_cost_cents  bigint NOT NULL CHECK (quoted_cost_cents >= 0),
    cost_cents         bigint CHECK (cost_cents >= 0),
    renewal_cost_cents bigint CHECK (renewal_cost_cents >= 0),
    register_attempts  smallint NOT NULL DEFAULT 0,
    attempted_at       timestamptz,
    error_code         text,
    error_message      text,
    registered_at      timestamptz,
    expires_at         timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (purchase_id, position),
    UNIQUE (purchase_id, domain_name)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS domain_purchase_items;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS domain_purchases;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS registrar_settings;
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM sources WHERE kind = 'cloudflare';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_kind_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_kind_check
    CHECK (kind IN ('apify', 'outscraper', 'emailable', 'instantly', 'mailchimp'));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_role_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_role_check
    CHECK (role IN ('maps', 'verifier', 'outreach', 'newsletter'));
-- +goose StatementEnd
