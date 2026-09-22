-- +goose Up

-- One row per distinct address: the same address can belong to several businesses,
-- and the 90-day third-party cache must be keyed by the address, not by the pair.
-- +goose StatementBegin
CREATE TABLE email_verifications (
    id                uuid PRIMARY KEY,
    email             citext NOT NULL UNIQUE,
    domain            text NOT NULL,
    pass1_score       integer NOT NULL DEFAULT 0 CHECK (pass1_score BETWEEN 0 AND 85),
    pass1_checks      jsonb NOT NULL DEFAULT '[]'::jsonb,
    pass1_hard_fail   text,
    pass1_verified_at timestamptz,
    pass2_score       integer CHECK (pass2_score IN (0, 70, 100)),
    pass2_status      text CHECK (pass2_status IN ('deliverable', 'risky', 'unknown',
                                                   'undeliverable', 'error')),
    pass2_raw         jsonb,
    pass2_source_id   uuid REFERENCES sources (id) ON DELETE SET NULL,
    pass2_credits     integer NOT NULL DEFAULT 0 CHECK (pass2_credits >= 0),
    pass2_verified_at timestamptz,
    final_score       integer NOT NULL DEFAULT 0 CHECK (final_score BETWEEN 0 AND 100),
    verification_tag  text NOT NULL DEFAULT 'red'
                      CHECK (verification_tag IN ('green', 'light_green', 'yellow',
                                                  'orange', 'red')),
    typo_suggestion   text,
    last_error        text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_verifications_tag_idx ON email_verifications (verification_tag, final_score DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_verifications_score_idx ON email_verifications (final_score DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_verifications_domain_idx ON email_verifications (domain);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_verifications_email_trgm_idx ON email_verifications
    USING gin ((email::text) gin_trgm_ops);
-- +goose StatementEnd
-- Finds the rows a third-party run may skip because their cache is still fresh.
-- +goose StatementBegin
CREATE INDEX email_verifications_pass2_idx ON email_verifications (pass2_verified_at)
    WHERE pass2_verified_at IS NOT NULL;
-- +goose StatementEnd
-- Finds the rows that still qualify for a paid run.
-- +goose StatementBegin
CREATE INDEX email_verifications_qualifying_idx ON email_verifications (pass1_score)
    WHERE pass1_verified_at IS NOT NULL;
-- +goose StatementEnd

-- Per-domain DNS and RDAP cache, shared by every worker replica.
-- +goose StatementBegin
CREATE TABLE verification_domains (
    domain          text PRIMARY KEY,
    has_mx          boolean NOT NULL DEFAULT false,
    has_a           boolean NOT NULL DEFAULT false,
    has_spf         boolean NOT NULL DEFAULT false,
    has_dmarc       boolean NOT NULL DEFAULT false,
    mx_hosts        text[] NOT NULL DEFAULT '{}',
    registered_at   timestamptz,
    rdap_checked_at timestamptz,
    dns_checked_at  timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE verification_runs (
    id             uuid PRIMARY KEY,
    pass           text NOT NULL CHECK (pass IN ('self', 'third_party')),
    status         text NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued', 'running', 'done', 'failed', 'cancelled')),
    filter         jsonb NOT NULL DEFAULT '{}'::jsonb,
    total          integer NOT NULL DEFAULT 0,
    done           integer NOT NULL DEFAULT 0,
    failed         integer NOT NULL DEFAULT 0,
    skipped        integer NOT NULL DEFAULT 0,
    credits_used   integer NOT NULL DEFAULT 0,
    est_cost_cents bigint NOT NULL DEFAULT 0,
    source_id      uuid REFERENCES sources (id) ON DELETE RESTRICT,
    error          text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    started_at     timestamptz,
    finished_at    timestamptz
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX verification_runs_created_idx ON verification_runs (created_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX verification_runs_status_idx ON verification_runs (status);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX verification_runs_pass_idx ON verification_runs (pass, created_at DESC);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE verification_run_items (
    run_id          uuid NOT NULL REFERENCES verification_runs (id) ON DELETE CASCADE,
    verification_id uuid NOT NULL REFERENCES email_verifications (id) ON DELETE CASCADE,
    status          text NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued', 'done', 'failed', 'skipped')),
    error           text,
    credits         integer NOT NULL DEFAULT 0 CHECK (credits >= 0),
    finished_at     timestamptz,
    PRIMARY KEY (run_id, verification_id)
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX verification_run_items_status_idx ON verification_run_items (run_id, status);
-- +goose StatementEnd

-- A source is either a Maps data provider or an email verifier. The verifier reuses
-- the existing encrypted-key storage, cost field and connection test.
-- +goose StatementBegin
ALTER TABLE sources ADD COLUMN role text NOT NULL DEFAULT 'maps'
    CHECK (role IN ('maps', 'verifier'));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT sources_kind_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_kind_check
    CHECK (kind IN ('apify', 'outscraper', 'emailable'));
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO sources (id, kind, name, role, cost_per_1k_cents, enabled)
VALUES ('0192f000-0000-7000-8000-000000000003', 'emailable',
        'Emailable · Email verification', 'verifier', 500, false)
ON CONFLICT (kind) DO NOTHING;
-- +goose StatementEnd

-- Verification now lives on the address, not on the business/address pair.
-- +goose StatementBegin
ALTER TABLE business_emails DROP COLUMN verified_status;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE business_emails ADD COLUMN verified_status text;
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM sources WHERE kind = 'emailable';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT sources_kind_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_kind_check CHECK (kind IN ('apify', 'outscraper'));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP COLUMN role;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS verification_run_items;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS verification_runs;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS verification_domains;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS email_verifications;
-- +goose StatementEnd
