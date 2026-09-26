-- name: GetGlobalExclusion :one
SELECT * FROM global_exclusions WHERE id = $1;

-- name: FindActiveGlobalExclusion :one
SELECT * FROM global_exclusions WHERE kind = $1 AND value = $2 AND removed_at IS NULL;

-- name: CreateGlobalExclusion :one
INSERT INTO global_exclusions (id, kind, value, display_value, match_mode, reason, source, source_ref_id)
VALUES (sqlc.arg('id'), sqlc.arg('kind'), sqlc.arg('value'), sqlc.arg('display_value'), sqlc.arg('match_mode'),
        sqlc.narg('reason'), sqlc.arg('source'), sqlc.narg('source_ref_id'))
RETURNING *;

-- name: RemoveGlobalExclusion :one
UPDATE global_exclusions
SET removed_at = now(), removed_note = sqlc.narg('note')
WHERE id = sqlc.arg('id') AND removed_at IS NULL
RETURNING *;

-- name: ExclusionCompanyKey :one
-- The company match key is computed by the same function the generated columns
-- use, so a rule and the rows it should match can never be normalised differently.
SELECT coalesce(exclusion_company_key(sqlc.arg('name')::text), '')::text AS key;

-- name: ExcludeLiveCampaignLeads :many
-- The exclusion sweep: every lead that is still waiting or in flight and whose
-- contact is now globally excluded stops. Finished and replied leads are history
-- and are left as they are.
UPDATE campaign_leads cl
SET status = 'excluded', claimed_at = NULL, last_push_error = 'globally excluded', updated_at = now()
FROM contacts c
WHERE c.id = cl.contact_id
  AND cl.status IN ('pending', 'pushing', 'active', 'paused')
  AND EXISTS (SELECT 1 FROM global_excluded_addresses x WHERE x.email = c.email)
RETURNING cl.*;

-- name: RestoreExcludedCampaignLeads :many
-- When a rule is removed, a lead it took out that never reached the provider goes
-- back to pending, unless its contact is suppressed, another rule still covers it,
-- or its campaign is over. A lead that was pushed stays excluded: it was removed
-- from Instantly and has to be imported again deliberately.
UPDATE campaign_leads cl
SET status = 'pending', last_push_error = NULL, updated_at = now()
FROM contacts c, campaigns camp
WHERE c.id = cl.contact_id AND camp.id = cl.campaign_id
  AND cl.status = 'excluded' AND cl.pushed_at IS NULL AND cl.instantly_lead_id IS NULL
  AND camp.status NOT IN ('completed', 'failed', 'archived')
  AND c.suppressed_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM global_excluded_addresses x WHERE x.email = c.email)
RETURNING cl.*;
