-- +goose Up

-- A provider query stops being one blocking HTTP call and becomes a vendor-side run
-- we start, poll and then drain.
--
-- Two things forced this. A state-wide Apify run takes hours, and the run-sync
-- endpoint gives up after five minutes — the run keeps going and keeps billing, but
-- nothing is left holding its id, so the data is paid for and lost. And a run is the
-- unit of spend: every place it returns is charged, so the same place must never be
-- inside two runs of the same job. One run now covers one location and *all* the
-- job's search terms, which is why terms is an array here.
--
-- run_state is the claim, not a duplicate of status. It is what keeps a retry from
-- starting a second paid run for a row that already has one, and what counts how many
-- runs a vendor account has in flight.

-- +goose StatementBegin
ALTER TABLE job_queries
    ADD COLUMN terms           text[] NOT NULL DEFAULT '{}'::text[],
    ADD COLUMN dataset_id      text,
    ADD COLUMN run_state       text NOT NULL DEFAULT 'none'
                               CHECK (run_state IN ('none', 'starting', 'polling', 'ingesting', 'finished')),
    ADD COLUMN run_status      text,
    ADD COLUMN ingested_offset integer NOT NULL DEFAULT 0,
    ADD COLUMN resurrected     boolean NOT NULL DEFAULT false,
    ADD COLUMN cost_cents      bigint NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- Rows written before this migration ran one term each; term stays the human label
-- and terms becomes the authoritative list, so both shapes read the same way.
-- +goose StatementBegin
UPDATE job_queries SET terms = ARRAY[term] WHERE cardinality(terms) = 0;
-- +goose StatementEnd

-- Finished rows must not look like live runs to the slot counter.
-- +goose StatementBegin
UPDATE job_queries SET run_state = 'finished'
WHERE status IN ('done', 'failed', 'cancelled');
-- +goose StatementEnd

-- The counter behind the concurrency cap reads only live runs, so the index carries
-- only those rows.
-- +goose StatementBegin
CREATE INDEX job_queries_live_runs_idx ON job_queries (job_id)
    WHERE run_state IN ('starting', 'polling');
-- +goose StatementEnd

-- Adopting an orphaned vendor run looks up run ids we already know about.
-- +goose StatementBegin
CREATE INDEX job_queries_provider_run_idx ON job_queries (provider_run_id)
    WHERE provider_run_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS job_queries_provider_run_idx;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS job_queries_live_runs_idx;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE job_queries
    DROP COLUMN terms,
    DROP COLUMN dataset_id,
    DROP COLUMN run_state,
    DROP COLUMN run_status,
    DROP COLUMN ingested_offset,
    DROP COLUMN resurrected,
    DROP COLUMN cost_cents;
-- +goose StatementEnd
