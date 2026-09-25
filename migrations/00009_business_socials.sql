-- +goose Up

-- The social profiles a business's website links to, found by the crawl stage.
--
-- They are kept even when the site yields no email: a Facebook or Instagram page is
-- often the only other way to reach a small business. url is the canonical profile
-- URL, so the same profile linked from every page footer, or as m.facebook.com and
-- www.facebook.com, is stored once per business.

-- +goose StatementBegin
CREATE TABLE business_socials (
    id          uuid PRIMARY KEY,
    business_id uuid NOT NULL REFERENCES businesses (id) ON DELETE CASCADE,
    network     text NOT NULL CHECK (network IN ('facebook', 'instagram', 'tiktok', 'youtube',
                                                 'linkedin', 'x', 'threads', 'pinterest',
                                                 'yelp', 'linktree')),
    handle      text NOT NULL,
    url         text NOT NULL,
    page_url    text,
    found_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, url)
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX business_socials_network_handle_idx ON business_socials (network, handle);
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP TABLE IF EXISTS business_socials;
-- +goose StatementEnd
