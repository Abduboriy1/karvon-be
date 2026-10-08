-- +goose Up
-- Two operator settings that used to need a restart or a click.
--
-- sources.max_active_runs replaces KARVON_PROVIDER_MAX_ACTIVE_RUNS. The cap belongs to
-- the vendor account (an Apify plan's memory limit), so it lives on the row that holds
-- that account's key and can change without a restart. A query that finds every slot
-- taken waits in the queue and starts when one frees up, whichever job it belongs to.
--
-- verification_settings.auto_self_verify makes the free self pass run on its own: a
-- periodic sweep starts a self run over every address that has never been scored, so
-- emails are verified as scrapes find them. The paid pass stays manual.
--
-- verification_runs.auto marks the runs that sweep started, so run history can tell
-- them apart from the ones an operator asked for.

-- +goose StatementBegin
ALTER TABLE sources
    ADD COLUMN max_active_runs integer NOT NULL DEFAULT 6
        CHECK (max_active_runs BETWEEN 1 AND 64);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE verification_settings
    ADD COLUMN auto_self_verify boolean NOT NULL DEFAULT true;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE verification_runs
    ADD COLUMN auto boolean NOT NULL DEFAULT false;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
ALTER TABLE verification_runs DROP COLUMN IF EXISTS auto;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE verification_settings DROP COLUMN IF EXISTS auto_self_verify;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP COLUMN IF EXISTS max_active_runs;
-- +goose StatementEnd
