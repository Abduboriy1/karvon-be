-- +goose Up
-- 'contains' match mode for global exclusions, so one rule covers a brand whose
-- locations each have their own name and domain: "Greater Houston YMCA" at
-- houstonymca.org and "YMCA of Callaway County" at ymcaofcallaway.org.
--
--   company       the rule's words appear anywhere in the normalised name, on word
--                 boundaries: "ymca" matches "osage prairie ymca" but "gold" does not
--                 match "marigold fitness".
--   domain        the rule's text appears anywhere in the website host or in the
--                 domain of an address: "ymca" matches orymca.org and
--                 ymca.kansascity.org. Covers the business and every address, like
--                 an exact domain rule.
--   email_domain  as domain, for addresses only.
--
-- The views stay the one definition of "excluded"; this migration restates them with
-- the new branches. Trigram indexes keep each contains rule an index lookup rather
-- than a scan of every row.

-- +goose StatementBegin
ALTER TABLE global_exclusions DROP CONSTRAINT IF EXISTS global_exclusions_match_mode_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE global_exclusions DROP CONSTRAINT IF EXISTS global_exclusions_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE global_exclusions
    ADD CONSTRAINT global_exclusions_match_mode_check
        CHECK (match_mode IN ('exact', 'prefix', 'contains')),
    ADD CONSTRAINT global_exclusions_check
        CHECK (match_mode = 'exact'
            OR (match_mode = 'prefix' AND kind = 'company')
            OR (match_mode = 'contains' AND kind IN ('company', 'domain', 'email_domain')));
-- +goose StatementEnd

-- domain_suffixes[1] is the full normalised host, so the domain indexes are on it.
-- +goose StatementBegin
CREATE INDEX businesses_name_key_trgm_idx ON businesses USING gin (name_key gin_trgm_ops);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX businesses_host_trgm_idx ON businesses USING gin ((domain_suffixes[1]) gin_trgm_ops);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX business_emails_host_trgm_idx ON business_emails USING gin ((domain_suffixes[1]) gin_trgm_ops);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_verifications_host_trgm_idx ON email_verifications USING gin ((domain_suffixes[1]) gin_trgm_ops);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contacts_host_trgm_idx ON contacts USING gin ((domain_suffixes[1]) gin_trgm_ops);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contacts_website_host_trgm_idx ON contacts USING gin ((website_suffixes[1]) gin_trgm_ops);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contacts_company_key_trgm_idx ON contacts USING gin (company_key gin_trgm_ops);
-- +goose StatementEnd

-- Rule values are validated to [a-z0-9 .-] for these modes, so they never carry a
-- LIKE wildcard. The plain LIKE on the key is what the trigram index serves; the
-- padded one enforces word boundaries.
-- +goose StatementBegin
CREATE OR REPLACE VIEW global_excluded_businesses AS
    SELECT b.id AS business_id, ge.id AS exclusion_id
    FROM global_exclusions ge
    JOIN businesses b ON b.domain_suffixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind = 'domain' AND ge.match_mode = 'exact'
    UNION ALL
    SELECT b.id, ge.id
    FROM global_exclusions ge
    JOIN businesses b ON b.domain_suffixes[1] LIKE '%' || ge.value || '%'
    WHERE ge.removed_at IS NULL AND ge.kind = 'domain' AND ge.match_mode = 'contains'
    UNION ALL
    SELECT b.id, ge.id
    FROM global_exclusions ge
    JOIN businesses b ON b.name_prefixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind = 'company'
      AND (ge.match_mode = 'prefix' OR (ge.match_mode = 'exact' AND b.name_key = ge.value))
    UNION ALL
    SELECT b.id, ge.id
    FROM global_exclusions ge
    JOIN businesses b ON b.name_key LIKE '%' || ge.value || '%'
                     AND ' ' || b.name_key || ' ' LIKE '% ' || ge.value || ' %'
    WHERE ge.removed_at IS NULL AND ge.kind = 'company' AND ge.match_mode = 'contains';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE VIEW global_excluded_addresses AS
    SELECT ge.value::citext AS email, ge.id AS exclusion_id
    FROM global_exclusions ge
    WHERE ge.removed_at IS NULL AND ge.kind = 'email'
    UNION ALL
    SELECT be.email, ge.id
    FROM global_exclusions ge
    JOIN business_emails be ON be.domain_suffixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind IN ('email_domain', 'domain') AND ge.match_mode = 'exact'
    UNION ALL
    SELECT be.email, ge.id
    FROM global_exclusions ge
    JOIN business_emails be ON be.domain_suffixes[1] LIKE '%' || ge.value || '%'
    WHERE ge.removed_at IS NULL AND ge.kind IN ('email_domain', 'domain') AND ge.match_mode = 'contains'
    UNION ALL
    SELECT ev.email, ge.id
    FROM global_exclusions ge
    JOIN email_verifications ev ON ev.domain_suffixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind IN ('email_domain', 'domain') AND ge.match_mode = 'exact'
    UNION ALL
    SELECT ev.email, ge.id
    FROM global_exclusions ge
    JOIN email_verifications ev ON ev.domain_suffixes[1] LIKE '%' || ge.value || '%'
    WHERE ge.removed_at IS NULL AND ge.kind IN ('email_domain', 'domain') AND ge.match_mode = 'contains'
    UNION ALL
    SELECT c.email, ge.id
    FROM global_exclusions ge
    JOIN contacts c ON c.domain_suffixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind IN ('email_domain', 'domain') AND ge.match_mode = 'exact'
    UNION ALL
    SELECT c.email, ge.id
    FROM global_exclusions ge
    JOIN contacts c ON c.domain_suffixes[1] LIKE '%' || ge.value || '%'
    WHERE ge.removed_at IS NULL AND ge.kind IN ('email_domain', 'domain') AND ge.match_mode = 'contains'
    UNION ALL
    SELECT be.email, xb.exclusion_id
    FROM global_excluded_businesses xb
    JOIN business_emails be ON be.business_id = xb.business_id
    UNION ALL
    SELECT c.email, xb.exclusion_id
    FROM global_excluded_businesses xb
    JOIN contacts c ON c.business_id = xb.business_id
    UNION ALL
    SELECT c.email, ge.id
    FROM global_exclusions ge
    JOIN contacts c ON c.website_suffixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind = 'domain' AND ge.match_mode = 'exact'
    UNION ALL
    SELECT c.email, ge.id
    FROM global_exclusions ge
    JOIN contacts c ON c.website_suffixes[1] LIKE '%' || ge.value || '%'
    WHERE ge.removed_at IS NULL AND ge.kind = 'domain' AND ge.match_mode = 'contains'
    UNION ALL
    SELECT c.email, ge.id
    FROM global_exclusions ge
    JOIN contacts c ON c.company_prefixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind = 'company'
      AND (ge.match_mode = 'prefix' OR (ge.match_mode = 'exact' AND c.company_key = ge.value))
    UNION ALL
    SELECT c.email, ge.id
    FROM global_exclusions ge
    JOIN contacts c ON c.company_key LIKE '%' || ge.value || '%'
                   AND ' ' || c.company_key || ' ' LIKE '% ' || ge.value || ' %'
    WHERE ge.removed_at IS NULL AND ge.kind = 'company' AND ge.match_mode = 'contains';
-- +goose StatementEnd

-- +goose Down
-- Contains rules cannot be expressed by the old schema; they are retired, not
-- deleted, so the history stays.
-- +goose StatementBegin
UPDATE global_exclusions SET match_mode = 'exact', removed_at = coalesce(removed_at, now()),
    removed_note = coalesce(removed_note, 'contains match mode rolled back')
WHERE match_mode = 'contains';
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE VIEW global_excluded_businesses AS
    SELECT b.id AS business_id, ge.id AS exclusion_id
    FROM global_exclusions ge
    JOIN businesses b ON b.domain_suffixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind = 'domain'
    UNION ALL
    SELECT b.id, ge.id
    FROM global_exclusions ge
    JOIN businesses b ON b.name_prefixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind = 'company'
      AND (ge.match_mode = 'prefix' OR b.name_key = ge.value);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE VIEW global_excluded_addresses AS
    SELECT ge.value::citext AS email, ge.id AS exclusion_id
    FROM global_exclusions ge
    WHERE ge.removed_at IS NULL AND ge.kind = 'email'
    UNION ALL
    SELECT be.email, ge.id
    FROM global_exclusions ge
    JOIN business_emails be ON be.domain_suffixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind IN ('email_domain', 'domain')
    UNION ALL
    SELECT ev.email, ge.id
    FROM global_exclusions ge
    JOIN email_verifications ev ON ev.domain_suffixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind IN ('email_domain', 'domain')
    UNION ALL
    SELECT c.email, ge.id
    FROM global_exclusions ge
    JOIN contacts c ON c.domain_suffixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind IN ('email_domain', 'domain')
    UNION ALL
    SELECT be.email, xb.exclusion_id
    FROM global_excluded_businesses xb
    JOIN business_emails be ON be.business_id = xb.business_id
    UNION ALL
    SELECT c.email, xb.exclusion_id
    FROM global_excluded_businesses xb
    JOIN contacts c ON c.business_id = xb.business_id
    UNION ALL
    SELECT c.email, ge.id
    FROM global_exclusions ge
    JOIN contacts c ON c.website_suffixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind = 'domain'
    UNION ALL
    SELECT c.email, ge.id
    FROM global_exclusions ge
    JOIN contacts c ON c.company_prefixes @> ARRAY[ge.value]
    WHERE ge.removed_at IS NULL AND ge.kind = 'company'
      AND (ge.match_mode = 'prefix' OR c.company_key = ge.value);
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS contacts_company_key_trgm_idx, contacts_website_host_trgm_idx, contacts_host_trgm_idx,
    email_verifications_host_trgm_idx, business_emails_host_trgm_idx, businesses_host_trgm_idx,
    businesses_name_key_trgm_idx;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE global_exclusions DROP CONSTRAINT IF EXISTS global_exclusions_check,
    DROP CONSTRAINT IF EXISTS global_exclusions_match_mode_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE global_exclusions
    ADD CONSTRAINT global_exclusions_match_mode_check CHECK (match_mode IN ('exact', 'prefix')),
    ADD CONSTRAINT global_exclusions_check CHECK (match_mode = 'exact' OR kind = 'company');
-- +goose StatementEnd
