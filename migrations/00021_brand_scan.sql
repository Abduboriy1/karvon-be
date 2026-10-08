-- +goose Up
-- The brand scan (GET /exclusions/brand-scan) groups stored businesses by name, by
-- the first words of the name, or by registrable website domain, and lists the
-- groups with many locations: chains and franchises the outreach is not after.
-- The scan itself is a read over businesses and needs no schema; this table holds
-- the groups an operator has looked at and decided to keep, so a scan converges on
-- the brands still to deal with instead of showing the same small multi-location
-- business every time.
--
-- A dismissal is keyed by the grouping and the group key the scan returned, so
-- dismissing the name group "joes pizza" does not hide the domain group
-- joespizza.com. Removing a dismissal deletes it: it records a preference, not an
-- action on any record, so there is nothing to audit.
-- +goose StatementBegin
CREATE TABLE brand_scan_dismissals (
    id         uuid PRIMARY KEY,
    group_by   text NOT NULL CHECK (group_by IN ('name', 'name_prefix', 'domain')),
    key        text NOT NULL CHECK (length(key) BETWEEN 1 AND 320),
    note       text CHECK (length(note) <= 1000),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (group_by, key)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS brand_scan_dismissals;
-- +goose StatementEnd
