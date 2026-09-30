-- +goose Up
-- Workspace: Google Workspace mailboxes on domains whose DNS is in the Cloudflare
-- account.
--
-- One Google Workspace account (tenant) holds every domain as a secondary domain. For
-- each domain Karvon adds it to Workspace, publishes the verification, MX, SPF and
-- DMARC records through Cloudflare, has Google verify it, and creates the mailboxes.
-- DKIM is the one step Google has no API for: the operator generates the key in the
-- Admin console and hands it back, and Karvon publishes it.
--
-- Every mailbox is a paid Workspace licence, so a setup is explicitly confirmed and
-- holds at most five mailboxes (workspace_mailboxes.position is 0-4 and unique within
-- its domain). Google's own uniqueness of an email address is what makes creating a
-- mailbox safe to repeat after a lost answer.
--
-- Every enum is text plus a CHECK constraint mirrored by Go constants
-- (internal/workspace/workspace.go).

-- The service-account key reuses the sources table: one encrypted-key path, one
-- connection test.
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_role_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_role_check
    CHECK (role IN ('maps', 'verifier', 'outreach', 'newsletter', 'registrar', 'mailboxes'));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_kind_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_kind_check
    CHECK (kind IN ('apify', 'outscraper', 'emailable', 'instantly', 'mailchimp', 'cloudflare', 'google_workspace'));
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO sources (id, kind, name, role, cost_per_1k_cents, enabled)
VALUES ('0192f000-0000-7000-8000-000000000007', 'google_workspace', 'Google Workspace · Mailboxes', 'mailboxes', 0, false)
ON CONFLICT (kind) DO NOTHING;
-- +goose StatementEnd

-- The parts of the connection that are not secret: the super admin the service
-- account acts as, and the service account's own identity, which the operator needs
-- to set up domain-wide delegation in the Admin console.
-- +goose StatementBegin
CREATE TABLE workspace_settings (
    id                        smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    admin_email               text CHECK (admin_email ~ '^[^@\s]+@[^@\s]+\.[^@\s]+$'),
    service_account_email     text,
    service_account_client_id text,
    updated_at                timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO workspace_settings (id) VALUES (1) ON CONFLICT DO NOTHING;
-- +goose StatementEnd

-- One domain being set up in Workspace.
--
--   provisioning   the worker is adding it, publishing DNS, waiting for Google to
--                  verify it, or creating mailboxes
--   dkim_required  everything Google has an API for is done; the DKIM key has to be
--                  generated in the Admin console and handed back
--   active         the DKIM record is published
--   failed         something needs a person; error_code and error_message say what,
--                  and a retry picks up where it stopped
-- +goose StatementBegin
CREATE TABLE workspace_domains (
    id                 uuid PRIMARY KEY,
    domain_name        text NOT NULL UNIQUE CHECK (length(domain_name) BETWEEN 3 AND 253),
    status             text NOT NULL DEFAULT 'provisioning'
                       CHECK (status IN ('provisioning', 'dkim_required', 'active', 'failed')),
    -- The TXT value Google asked for, kept so every pass publishes the same one.
    verification_token text,
    dkim_selector      text,
    -- Milestones, each set once the step is done so a retry skips it.
    added_at           timestamptz,
    dns_published_at   timestamptz,
    verified_at        timestamptz,
    dkim_published_at  timestamptz,
    error_code         text,
    error_message      text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX workspace_domains_created_at_idx ON workspace_domains (created_at DESC);
-- +goose StatementEnd

-- One mailbox (a Workspace user, and a paid licence) on a domain.
--
--   pending  not created yet
--   created  the user exists in Workspace with the password stored here
--   failed   Google refused it, or the address already belonged to someone else
--
-- A created mailbox can then be connected to Instantly through Instantly's Google
-- OAuth flow: Karvon starts a session, a person signs in as the mailbox, and Karvon
-- follows the session until it ends.
--
--   instantly_status NULL        never connected
--                    connecting  a session is open (it lives ten minutes)
--                    connected   Instantly has the account
--                    failed      Instantly refused it (instantly_error says why)
--                    expired     nobody signed in before the session ran out
-- +goose StatementBegin
CREATE TABLE workspace_mailboxes (
    id              uuid PRIMARY KEY,
    domain_id       uuid NOT NULL REFERENCES workspace_domains (id) ON DELETE CASCADE,
    position        smallint NOT NULL CHECK (position BETWEEN 0 AND 4),
    email           text NOT NULL UNIQUE CHECK (email = lower(email) AND length(email) BETWEEN 3 AND 254),
    given_name      text NOT NULL CHECK (length(given_name) BETWEEN 1 AND 60),
    family_name     text NOT NULL CHECK (length(family_name) BETWEEN 1 AND 60),
    -- The initial password, AES-GCM encrypted with KARVON_SECRET_KEY. It is what the
    -- operator signs in with to connect the mailbox to the sending tool.
    password_enc    bytea NOT NULL,
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'created', 'failed')),
    create_attempts smallint NOT NULL DEFAULT 0,
    google_user_id  text,
    error_code      text,
    error_message   text,
    provisioned_at  timestamptz,
    instantly_status             text CHECK (instantly_status IN ('connecting', 'connected', 'failed', 'expired')),
    instantly_session_id         text,
    instantly_auth_url           text,
    instantly_session_expires_at timestamptz,
    instantly_account_id         text,
    instantly_error              text,
    instantly_connected_at       timestamptz,
    -- Whether warmup was asked for with the connection, and when Instantly took it.
    instantly_warmup             boolean NOT NULL DEFAULT false,
    instantly_warmup_enabled_at  timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (domain_id, position)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS workspace_mailboxes;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS workspace_domains;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS workspace_settings;
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM sources WHERE kind = 'google_workspace';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_kind_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_kind_check
    CHECK (kind IN ('apify', 'outscraper', 'emailable', 'instantly', 'mailchimp', 'cloudflare'));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_role_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_role_check
    CHECK (role IN ('maps', 'verifier', 'outreach', 'newsletter', 'registrar'));
-- +goose StatementEnd
