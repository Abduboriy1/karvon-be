-- +goose Up
-- Scrape categories are named sets of search terms that pre-fill the Create scrape
-- form. They are user-editable; the two seeded rows are marked default and cannot be
-- deleted, only edited.
-- +goose StatementBegin
CREATE TABLE scrape_categories (
    id          uuid PRIMARY KEY,
    name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    terms       text[] NOT NULL CHECK (cardinality(terms) >= 1),
    is_default  boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX scrape_categories_name_key ON scrape_categories (lower(name));
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO scrape_categories (id, name, terms, is_default)
VALUES
    ('0192f000-0000-7000-8000-000000000101', 'Gyms', ARRAY[
        'gym', 'fitness center', 'crossfit box', 'personal trainer', 'boxing gym',
        'group fitness', 'fitness', 'fitness studio', 'hyrox', 'cycling',
        'supplements', 'nutrition store', 'nutrition', 'sports nutrition',
        'clinic', 'spa', 'chiropractor',
        'gun range', 'gun shops',
        'convenience store', 'convenience market',
        'smoothie shop', 'smoothie', 'cafe', 'sports complex'
    ], true),
    ('0192f000-0000-7000-8000-000000000102', 'Med spas', ARRAY[
        'med spa', 'medical spa', 'botox clinic', 'aesthetic clinic', 'laser hair removal'
    ], true)
ON CONFLICT (id) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS scrape_categories;
-- +goose StatementEnd
