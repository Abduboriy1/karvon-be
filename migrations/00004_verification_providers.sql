-- +goose Up

-- The free stage is no longer one local pipeline but a weighted combination of
-- several providers. The Pass 1 columns keep their meaning (the local pipeline's own
-- raw score and check breakdown); what is new is the combined score and the record of
-- how each provider contributed to it.

-- +goose StatementBegin
ALTER TABLE email_verifications
    ADD COLUMN free_score integer NOT NULL DEFAULT 0 CHECK (free_score BETWEEN 0 AND 89),
    ADD COLUMN free_scored_at timestamptz,
    ADD COLUMN provider_results jsonb NOT NULL DEFAULT '[]'::jsonb;
-- +goose StatementEnd

-- Backfill so an address that already passed the local pipeline stays eligible for
-- exactly the paid runs it was eligible for before. The old scale topped out at 85;
-- rescaling onto 0-100 and clamping to the free ceiling is the same normalization the
-- code applies to a fresh local result.
-- +goose StatementBegin
UPDATE email_verifications
SET free_score     = LEAST(89, ROUND(pass1_score * 100.0 / 85)::integer),
    free_scored_at = pass1_verified_at
WHERE pass1_verified_at IS NOT NULL;
-- +goose StatementEnd

-- The paid gate now reads free_score rather than pass1_score.
-- +goose StatementBegin
CREATE INDEX email_verifications_free_score_idx ON email_verifications (free_score)
    WHERE free_scored_at IS NOT NULL;
-- +goose StatementEnd

-- One row of operator-editable policy. Weights and enablement are JSONB keyed by
-- provider, so adding a fourth provider later is a code change and a settings edit
-- rather than another migration.
-- +goose StatementBegin
CREATE TABLE verification_settings (
    id             smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    weights        jsonb NOT NULL DEFAULT '{}'::jsonb,
    enabled        jsonb NOT NULL DEFAULT '{}'::jsonb,
    paid_enabled   boolean NOT NULL DEFAULT true,
    paid_threshold integer NOT NULL DEFAULT 75 CHECK (paid_threshold BETWEEN 0 AND 100),
    paid_min_score integer NOT NULL DEFAULT 50 CHECK (paid_min_score BETWEEN 0 AND 100),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- The row is created empty on purpose: the service seeds it from the KARVON_*
-- environment defaults the first time it is read, so an existing deployment keeps its
-- current behaviour without anyone editing SQL.
-- +goose StatementBegin
INSERT INTO verification_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS verification_settings;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS email_verifications_free_score_idx;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE email_verifications
    DROP COLUMN IF EXISTS provider_results,
    DROP COLUMN IF EXISTS free_scored_at,
    DROP COLUMN IF EXISTS free_score;
-- +goose StatementEnd
