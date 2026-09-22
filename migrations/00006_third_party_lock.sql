-- +goose Up

-- An address may be sent to a third-party verifier exactly once, ever.
--
-- Until now the guard was a 90-day cache window: a conclusive result suppressed a
-- second charge until it expired, and an inconclusive one suppressed nothing at all,
-- so the same address could reach a provider again. The rule is now absolute, and it
-- is recorded at send time rather than at result time, because what must never happen
-- twice is the outbound call — not the verdict it happens to return.
--
-- third_party_sent_at is claimed immediately before the call and cleared again only
-- when the call never reached the provider (a rejected API key, a throttle, a
-- transport failure). Any answer the provider gives — deliverable, risky,
-- undeliverable, unknown, or its own error — keeps the lock forever.

-- +goose StatementBegin
ALTER TABLE email_verifications ADD COLUMN third_party_sent_at timestamptz;
-- +goose StatementEnd

-- Backfill every address that demonstrably reached a provider: one that stored a
-- conclusive result, one that was billed, and one carrying a verdict the provider
-- itself produced. 'error' is deliberately excluded — historically it was also
-- written when our own retries ran out without the provider ever answering, and
-- locking those would burn addresses that were never actually checked.
-- +goose StatementBegin
UPDATE email_verifications
SET third_party_sent_at = COALESCE(pass2_verified_at, updated_at)
WHERE pass2_verified_at IS NOT NULL
   OR pass2_credits > 0
   OR pass2_status IN ('deliverable', 'risky', 'undeliverable', 'unknown');
-- +goose StatementEnd

-- The paid gate is now "never sent", so that is what the index covers.
-- +goose StatementBegin
CREATE INDEX email_verifications_never_sent_idx ON email_verifications (free_score)
    WHERE third_party_sent_at IS NULL;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS email_verifications_pass2_idx;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS email_verifications_qualifying_idx;
-- +goose StatementEnd

-- The application claims the lock with a conditional UPDATE, which is already atomic.
-- This trigger is the second line: no code path, migration or hand-run statement can
-- move a lock that is held, or write a second third-party verdict over a first one.
-- Releasing the claim is allowed only while no verdict was ever stored, which is
-- exactly the "the provider never answered" case.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION email_verifications_guard_third_party() RETURNS trigger AS $$
BEGIN
    IF OLD.pass2_verified_at IS NOT NULL
       AND NEW.pass2_verified_at IS DISTINCT FROM OLD.pass2_verified_at THEN
        RAISE EXCEPTION
            'email_verifications: % already holds a third-party result from %; a second one may never be written',
            OLD.email, OLD.pass2_verified_at
            USING ERRCODE = 'raise_exception';
    END IF;

    IF OLD.third_party_sent_at IS NOT NULL
       AND NEW.third_party_sent_at IS NOT NULL
       AND NEW.third_party_sent_at IS DISTINCT FROM OLD.third_party_sent_at THEN
        RAISE EXCEPTION
            'email_verifications: % was sent to a third party on %; the send lock may not be moved',
            OLD.email, OLD.third_party_sent_at
            USING ERRCODE = 'raise_exception';
    END IF;

    IF OLD.third_party_sent_at IS NOT NULL
       AND NEW.third_party_sent_at IS NULL
       AND OLD.pass2_verified_at IS NOT NULL THEN
        RAISE EXCEPTION
            'email_verifications: % holds a third-party result; the send lock may not be released',
            OLD.email
            USING ERRCODE = 'raise_exception';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER email_verifications_guard_third_party
    BEFORE UPDATE ON email_verifications
    FOR EACH ROW EXECUTE FUNCTION email_verifications_guard_third_party();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS email_verifications_guard_third_party ON email_verifications;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS email_verifications_guard_third_party();
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_verifications_qualifying_idx ON email_verifications (pass1_score)
    WHERE pass1_verified_at IS NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_verifications_pass2_idx ON email_verifications (pass2_verified_at)
    WHERE pass2_verified_at IS NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS email_verifications_never_sent_idx;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE email_verifications DROP COLUMN third_party_sent_at;
-- +goose StatementEnd
