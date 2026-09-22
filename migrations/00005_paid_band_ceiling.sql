-- +goose Up

-- The paid ceiling moves from 75 to 90, one above the free ceiling, so every address
-- the free stage does not rule out is paid for once. A free score reads the domain,
-- not the mailbox: a catch-all domain and a departed employee at a well-configured
-- one both score high, and only a paid result can reach the green band at all.

-- +goose StatementBegin
ALTER TABLE verification_settings
    ALTER COLUMN paid_threshold SET DEFAULT 90;
-- +goose StatementEnd

-- Only a row still holding the old default is moved. An operator who chose their own
-- ceiling keeps it; this migration is not entitled to spend their credits for them.
-- +goose StatementBegin
UPDATE verification_settings
SET paid_threshold = 90,
    updated_at     = now()
WHERE paid_threshold = 75;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE verification_settings
    ALTER COLUMN paid_threshold SET DEFAULT 75;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE verification_settings
SET paid_threshold = 75,
    updated_at     = now()
WHERE paid_threshold = 90;
-- +goose StatementEnd
