-- +goose Up
-- Deterministic ids so the frontend can be developed against a fresh database and
-- so re-running migrations never duplicates a provider row.
-- +goose StatementBegin
INSERT INTO sources (id, kind, name, cost_per_1k_cents, enabled)
VALUES
    ('0192f000-0000-7000-8000-000000000001', 'apify', 'Apify · Google Maps', 400, false),
    ('0192f000-0000-7000-8000-000000000002', 'outscraper', 'Outscraper · Google Maps', 300, false)
ON CONFLICT (kind) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM sources WHERE kind IN ('apify', 'outscraper');
-- +goose StatementEnd
