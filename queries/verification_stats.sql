-- name: VerificationCounters :one
SELECT (SELECT count(*) FROM email_verifications)::bigint                                          AS total,
       (SELECT count(*) FROM email_verifications WHERE verification_tag = 'green')::bigint         AS tag_green,
       (SELECT count(*) FROM email_verifications WHERE verification_tag = 'light_green')::bigint   AS tag_light_green,
       (SELECT count(*) FROM email_verifications WHERE verification_tag = 'yellow')::bigint        AS tag_yellow,
       (SELECT count(*) FROM email_verifications WHERE verification_tag = 'orange')::bigint        AS tag_orange,
       (SELECT count(*) FROM email_verifications WHERE verification_tag = 'red')::bigint           AS tag_red,
       (SELECT count(*) FROM email_verifications WHERE free_scored_at IS NOT NULL)::bigint         AS self_verified,
       (SELECT count(*) FROM email_verifications WHERE pass2_verified_at IS NOT NULL)::bigint      AS third_party_verified,
       (SELECT COALESCE(sum(pass2_credits), 0) FROM email_verifications)::bigint                   AS credits_used_total,
       (SELECT COALESCE(sum(credits_used), 0) FROM verification_runs
        WHERE created_at > now() - interval '30 days')::bigint                                     AS credits_used_30d;

-- name: CountEmailsNeedingSelfVerification :one
-- Addresses on the master list that have never been through the free stage, whether
-- or not a verification row exists for them yet.
SELECT count(DISTINCT be.email)::bigint
FROM business_emails be
         LEFT JOIN email_verifications ev ON ev.email = be.email
WHERE ev.id IS NULL OR ev.free_scored_at IS NULL;

-- name: CountQualifyingForThirdParty :one
-- Inside the paid band and never sent to a third party. The band has both a floor
-- (below it an address is not worth paying for) and a ceiling (at or above it the
-- free providers are already confident enough that paying adds nothing); the send
-- lock is absolute and outranks both.
SELECT count(*)::bigint
FROM email_verifications ev
WHERE ev.free_scored_at IS NOT NULL
  AND ev.third_party_sent_at IS NULL
  AND ev.free_score >= sqlc.arg('min_score')::int
  AND ev.free_score < sqlc.arg('max_score')::int;
