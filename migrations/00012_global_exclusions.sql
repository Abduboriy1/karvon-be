-- +goose Up
-- Global exclusions: businesses, addresses and domains that must never be verified,
-- enrolled, exported or contacted.
--
-- An exclusion is a rule, not a flag copied onto rows. Nothing scraped is rewritten
-- or deleted: whether a record is excluded is answered live by the two views below,
-- global_excluded_businesses and global_excluded_addresses, so a rule applies to
-- every existing and future record the moment it is saved and stops applying the
-- moment it is removed. The views are the one definition of "excluded"; Go code and
-- every query builder read them (internal/db/exclusions.go) rather than restating it.
--
-- Rule kinds:
--   company       the business name, normalised by exclusion_company_key. In
--                 'prefix' mode it also matches names that start with it word by
--                 word, which is how a chain's per-location listings are caught.
--   domain        the business website and every address at that domain,
--                 subdomains included.
--   email_domain  addresses at that domain (subdomains included) only.
--   email         one exact address.
-- An address is excluded when a rule matches it directly or when any business that
-- holds it is excluded.

-- exclusion_company_key folds a company name to its match key: lower case, "&" as
-- "and", apostrophes dropped, other punctuation as spaces, a leading "the" and
-- trailing legal suffixes removed. "The Planet-Fitness, Inc." -> "planet fitness".
-- +goose StatementBegin
CREATE FUNCTION exclusion_company_key(name text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT NULLIF(
        regexp_replace(
            regexp_replace(
                btrim(regexp_replace(
                    regexp_replace(replace(lower(coalesce(name, '')), '&', ' and '), '[''’`]', '', 'g'),
                    '[^[:alnum:]]+', ' ', 'g')),
                '^the ', ''),
            '( (incorporated|inc|llc|l l c|ltd|limited|corp|corporation|co|company|plc|gmbh|pllc|llp|lp|pc))+$', ''),
        '')
$$;
-- +goose StatementEnd

-- exclusion_host reduces a website or bare host to its lower-case host without a
-- leading "www.", or NULL.
-- +goose StatementBegin
CREATE FUNCTION exclusion_host(website text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT NULLIF(
        regexp_replace(
            regexp_replace(
                regexp_replace(lower(btrim(coalesce(website, ''))), '^[a-z][a-z0-9+.-]*://', ''),
                '[/:?#].*$', ''),
            '^www\.|\.$', '', 'g'),
        '')
$$;
-- +goose StatementEnd

-- exclusion_domain_suffixes lists a host and every parent domain of two or more
-- labels: "a.shop.example.com" -> {a.shop.example.com, shop.example.com, example.com}.
-- A domain rule matches when its value is one of these, which keeps every domain
-- check an index lookup.
-- +goose StatementBegin
CREATE FUNCTION exclusion_domain_suffixes(host text) RETURNS text[]
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT coalesce(array_agg(array_to_string(labels[i:], '.') ORDER BY i), '{}')
    FROM (SELECT string_to_array(exclusion_host(host), '.') AS labels) h,
         generate_subscripts(h.labels, 1) AS i
    WHERE i < cardinality(h.labels)
$$;
-- +goose StatementEnd

-- exclusion_name_prefixes lists the word prefixes of a company key:
-- "planet fitness austin" -> {planet, planet fitness, planet fitness austin}.
-- +goose StatementBegin
CREATE FUNCTION exclusion_name_prefixes(key text) RETURNS text[]
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT coalesce(array_agg(array_to_string(words[1:i], ' ') ORDER BY i), '{}')
    FROM (SELECT string_to_array(key, ' ') AS words) w,
         generate_subscripts(w.words, 1) AS i
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE global_exclusions (
    id            uuid PRIMARY KEY,
    kind          text NOT NULL CHECK (kind IN ('company', 'email', 'email_domain', 'domain')),
    value         text NOT NULL CHECK (length(value) BETWEEN 1 AND 320),
    display_value text NOT NULL CHECK (length(display_value) BETWEEN 1 AND 500),
    match_mode    text NOT NULL DEFAULT 'exact' CHECK (match_mode IN ('exact', 'prefix')),
    reason        text CHECK (length(reason) <= 1000),
    source        text NOT NULL DEFAULT 'manual'
                  CHECK (source IN ('manual', 'business', 'verification', 'lead', 'contact')),
    source_ref_id uuid,
    created_at    timestamptz NOT NULL DEFAULT now(),
    removed_at    timestamptz,
    removed_note  text,
    CHECK (match_mode = 'exact' OR kind = 'company')
);
-- +goose StatementEnd
-- One active rule per kind and value; removed rules stay as history.
-- +goose StatementBegin
CREATE UNIQUE INDEX global_exclusions_active_idx ON global_exclusions (kind, value) WHERE removed_at IS NULL;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX global_exclusions_created_idx ON global_exclusions (created_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX global_exclusions_display_trgm_idx ON global_exclusions USING gin (display_value gin_trgm_ops);
-- +goose StatementEnd

-- The match keys are stored as generated columns so every rule can find the rows it
-- covers through a GIN index, instead of every row being normalised on every query.
-- They are derived from name, domain, email, company and website and never written
-- directly.
-- +goose StatementBegin
ALTER TABLE businesses
    ADD COLUMN name_key        text   GENERATED ALWAYS AS (exclusion_company_key(name)) STORED,
    ADD COLUMN name_prefixes   text[] GENERATED ALWAYS AS (exclusion_name_prefixes(exclusion_company_key(name))) STORED,
    ADD COLUMN domain_suffixes text[] GENERATED ALWAYS AS (exclusion_domain_suffixes(domain)) STORED;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX businesses_name_prefixes_idx ON businesses USING gin (name_prefixes);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX businesses_domain_suffixes_idx ON businesses USING gin (domain_suffixes);
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE business_emails
    ADD COLUMN domain_suffixes text[] GENERATED ALWAYS AS (exclusion_domain_suffixes(split_part(email::text, '@', 2))) STORED;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX business_emails_domain_suffixes_idx ON business_emails USING gin (domain_suffixes);
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE email_verifications
    ADD COLUMN domain_suffixes text[] GENERATED ALWAYS AS (exclusion_domain_suffixes(domain)) STORED;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_verifications_domain_suffixes_idx ON email_verifications USING gin (domain_suffixes);
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE contacts
    ADD COLUMN domain_suffixes  text[] GENERATED ALWAYS AS (exclusion_domain_suffixes(domain)) STORED,
    ADD COLUMN website_suffixes text[] GENERATED ALWAYS AS (exclusion_domain_suffixes(website)) STORED,
    ADD COLUMN company_key      text   GENERATED ALWAYS AS (exclusion_company_key(company)) STORED,
    ADD COLUMN company_prefixes text[] GENERATED ALWAYS AS (exclusion_name_prefixes(exclusion_company_key(company))) STORED;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contacts_domain_suffixes_idx ON contacts USING gin (domain_suffixes);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contacts_website_suffixes_idx ON contacts USING gin (website_suffixes);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contacts_company_prefixes_idx ON contacts USING gin (company_prefixes);
-- +goose StatementEnd

-- global_excluded_businesses is every excluded business with the rule that
-- excludes it. It is driven from the rules, so its cost grows with the matches, not
-- with the size of the business list.
-- +goose StatementBegin
CREATE VIEW global_excluded_businesses AS
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

-- global_excluded_addresses is every known excluded address with the rule behind
-- it: exact addresses; addresses at an excluded domain wherever they are stored
-- (business emails, verification rows, contacts); every address of an excluded
-- business; and contacts whose own company or website is excluded. An address can
-- appear more than once, once per rule and path.
-- +goose StatementBegin
CREATE VIEW global_excluded_addresses AS
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

-- An excluded lead has been taken out of a campaign by a global exclusion. Unlike
-- suppressed, it says nothing about the contact, and a lead that never reached the
-- provider goes back to pending when the rule is removed.
-- +goose StatementBegin
ALTER TABLE campaign_leads DROP CONSTRAINT IF EXISTS campaign_leads_status_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaign_leads ADD CONSTRAINT campaign_leads_status_check
    CHECK (status IN ('pending', 'pushing', 'active', 'paused', 'completed', 'replied',
                      'bounced', 'unsubscribed', 'skipped', 'suppressed', 'failed', 'excluded'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE campaign_leads SET status = 'skipped' WHERE status = 'excluded';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaign_leads DROP CONSTRAINT IF EXISTS campaign_leads_status_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaign_leads ADD CONSTRAINT campaign_leads_status_check
    CHECK (status IN ('pending', 'pushing', 'active', 'paused', 'completed', 'replied',
                      'bounced', 'unsubscribed', 'skipped', 'suppressed', 'failed'));
-- +goose StatementEnd
-- +goose StatementBegin
DROP VIEW IF EXISTS global_excluded_addresses;
-- +goose StatementEnd
-- +goose StatementBegin
DROP VIEW IF EXISTS global_excluded_businesses;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE contacts DROP COLUMN IF EXISTS domain_suffixes, DROP COLUMN IF EXISTS website_suffixes,
    DROP COLUMN IF EXISTS company_key, DROP COLUMN IF EXISTS company_prefixes;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE email_verifications DROP COLUMN IF EXISTS domain_suffixes;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE business_emails DROP COLUMN IF EXISTS domain_suffixes;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE businesses DROP COLUMN IF EXISTS name_key, DROP COLUMN IF EXISTS name_prefixes,
    DROP COLUMN IF EXISTS domain_suffixes;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS global_exclusions;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS exclusion_name_prefixes(text);
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS exclusion_domain_suffixes(text);
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS exclusion_host(text);
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS exclusion_company_key(text);
-- +goose StatementEnd
