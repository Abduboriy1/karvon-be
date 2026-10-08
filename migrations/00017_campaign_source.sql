-- +goose Up
-- Where a campaign was started. 'karvon' campaigns are built here and launched
-- into Instantly; 'instantly' campaigns were started in Instantly's own app and
-- are imported by the campaign sync so the list shows every campaign the
-- workspace runs. An imported campaign is mirrored, not edited: its sequence,
-- leads and settings stay Instantly's.
-- +goose StatementBegin
ALTER TABLE campaigns
    ADD COLUMN source text NOT NULL DEFAULT 'karvon' CHECK (source IN ('karvon', 'instantly'));
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX campaigns_source_idx ON campaigns (source);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM campaigns WHERE source = 'instantly';
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS campaigns_source_idx;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaigns DROP COLUMN IF EXISTS source;
-- +goose StatementEnd
