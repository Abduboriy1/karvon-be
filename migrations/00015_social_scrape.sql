-- +goose Up
-- Social media scrapes: a job that reads the public page of each business's social
-- profile (Facebook for now, through services/fb-scrape) instead of its website.
--
-- The addresses it finds are ordinary business_emails rows with their own source, so
-- they can be told apart from what the website crawl found and filtered on.
-- +goose StatementBegin
ALTER TABLE business_emails DROP CONSTRAINT IF EXISTS business_emails_source_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE business_emails ADD CONSTRAINT business_emails_source_check
    CHECK (source IN ('mailto', 'regex', 'provider', 'facebook'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM business_emails WHERE source = 'facebook';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE business_emails DROP CONSTRAINT IF EXISTS business_emails_source_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE business_emails ADD CONSTRAINT business_emails_source_check
    CHECK (source IN ('mailto', 'regex', 'provider'));
-- +goose StatementEnd
