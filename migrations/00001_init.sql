-- +goose Up
-- +goose StatementBegin
CREATE EXTENSION IF NOT EXISTS citext;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE EXTENSION IF NOT EXISTS pg_trgm;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE sources (
    id                 uuid PRIMARY KEY,
    kind               text NOT NULL UNIQUE CHECK (kind IN ('apify', 'outscraper')),
    name               text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    api_key_enc        bytea,
    cost_per_1k_cents  integer NOT NULL DEFAULT 0 CHECK (cost_per_1k_cents >= 0),
    enabled            boolean NOT NULL DEFAULT false,
    last_tested_at     timestamptz,
    last_test_ok       boolean,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE jobs (
    id          uuid PRIMARY KEY,
    name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    status      text NOT NULL DEFAULT 'queued'
                CHECK (status IN ('queued', 'running', 'done', 'failed', 'cancelled')),
    source_id   uuid NOT NULL REFERENCES sources (id) ON DELETE RESTRICT,
    config      jsonb NOT NULL,
    stats       jsonb NOT NULL DEFAULT '{}'::jsonb,
    error       text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    finished_at timestamptz
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX jobs_created_at_idx ON jobs (created_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX jobs_status_idx ON jobs (status);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX jobs_source_id_idx ON jobs (source_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX jobs_name_trgm_idx ON jobs USING gin (name gin_trgm_ops);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE businesses (
    id              uuid PRIMARY KEY,
    place_id        text UNIQUE,
    name            text NOT NULL,
    category        text,
    address         text,
    city            text,
    state           text,
    zip             text,
    phone           text,
    website         text,
    domain          text,
    rating          double precision,
    reviews         integer,
    lat             double precision,
    lng             double precision,
    raw             jsonb,
    first_job_id    uuid REFERENCES jobs (id) ON DELETE SET NULL,
    suppressed      boolean NOT NULL DEFAULT false,
    notes           text,
    last_crawled_at timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX businesses_domain_idx ON businesses (domain) WHERE domain IS NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX businesses_state_category_idx ON businesses (state, category);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX businesses_city_idx ON businesses (city);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX businesses_created_at_idx ON businesses (created_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX businesses_name_trgm_idx ON businesses USING gin (name gin_trgm_ops);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX businesses_domain_trgm_idx ON businesses USING gin (domain gin_trgm_ops);
-- +goose StatementEnd
-- Dedupe fallback for listings that arrive without a Google place id.
-- +goose StatementBegin
CREATE INDEX businesses_phone_zip_idx ON businesses (phone, zip) WHERE place_id IS NULL;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX businesses_domain_crawled_idx ON businesses (domain, last_crawled_at DESC)
    WHERE domain IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE job_queries (
    id              uuid PRIMARY KEY,
    job_id          uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    term            text NOT NULL,
    city            text NOT NULL,
    state           text NOT NULL DEFAULT '',
    status          text NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued', 'running', 'done', 'failed', 'cancelled')),
    listings_found  integer NOT NULL DEFAULT 0,
    provider_run_id text,
    error           text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,
    finished_at     timestamptz,
    UNIQUE (job_id, term, city, state)
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX job_queries_job_status_idx ON job_queries (job_id, status);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE job_results (
    job_id      uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    business_id uuid NOT NULL REFERENCES businesses (id) ON DELETE CASCADE,
    query_id    uuid REFERENCES job_queries (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, business_id)
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX job_results_business_idx ON job_results (business_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX job_results_query_idx ON job_results (query_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE business_emails (
    id              uuid PRIMARY KEY,
    business_id     uuid NOT NULL REFERENCES businesses (id) ON DELETE CASCADE,
    email           citext NOT NULL,
    source          text NOT NULL CHECK (source IN ('mailto', 'regex', 'provider')),
    page_url        text,
    is_primary      boolean NOT NULL DEFAULT false,
    verified_status text,
    found_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, email)
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX business_emails_email_idx ON business_emails (email);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX business_emails_one_primary_idx ON business_emails (business_id)
    WHERE is_primary;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE job_events (
    id     bigserial PRIMARY KEY,
    job_id uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    ts     timestamptz NOT NULL DEFAULT now(),
    type   text NOT NULL CHECK (type IN ('progress', 'log', 'status')),
    data   jsonb NOT NULL
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX job_events_job_id_idx ON job_events (job_id, id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX job_events_ts_idx ON job_events (ts);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS job_events;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS business_emails;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS job_results;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS job_queries;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS businesses;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS jobs;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS sources;
-- +goose StatementEnd
