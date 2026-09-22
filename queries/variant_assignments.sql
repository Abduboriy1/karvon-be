-- name: CreateVariantAssignment :one
INSERT INTO variant_assignments (id, campaign_lead_id, step, variant_id, weights_version, seed_hash, rendered_subject,
                                 rendered_body, component_ids, subject_component_id, hook_component_id, cta_component_id)
VALUES (sqlc.arg('id'), sqlc.arg('campaign_lead_id'), sqlc.arg('step'), sqlc.arg('variant_id'), sqlc.arg('weights_version'),
        sqlc.arg('seed_hash'), sqlc.arg('rendered_subject'), sqlc.arg('rendered_body'), sqlc.arg('component_ids'),
        sqlc.narg('subject_component_id'), sqlc.narg('hook_component_id'), sqlc.narg('cta_component_id'))
ON CONFLICT (campaign_lead_id, step) DO UPDATE SET assigned_at = variant_assignments.assigned_at
RETURNING *;

-- name: GetVariantAssignment :one
SELECT * FROM variant_assignments WHERE campaign_lead_id = $1 AND step = $2;

-- name: GetVariantAssignmentByID :one
SELECT * FROM variant_assignments WHERE id = $1;

-- name: ListVariantAssignmentsForLead :many
SELECT * FROM variant_assignments WHERE campaign_lead_id = $1 ORDER BY step;

-- name: LockVariantAssignments :exec
UPDATE variant_assignments SET locked_at = now() WHERE campaign_lead_id = $1 AND locked_at IS NULL;

-- name: DeleteUnlockedAssignmentsForLead :exec
DELETE FROM variant_assignments WHERE campaign_lead_id = $1 AND locked_at IS NULL;

-- name: CountLockedAssignmentsForCampaign :one
SELECT count(*) FROM variant_assignments va
JOIN campaign_leads cl ON cl.id = va.campaign_lead_id
WHERE cl.campaign_id = $1 AND va.locked_at IS NOT NULL;

-- name: CountLockedAssignmentsForCampaignVariant :one
SELECT count(*) FROM variant_assignments va
JOIN campaign_leads cl ON cl.id = va.campaign_lead_id
WHERE cl.campaign_id = $1 AND va.variant_id = $2 AND va.locked_at IS NOT NULL;

-- name: CountAssignmentsByVariant :many
SELECT va.variant_id, va.step, count(*)::bigint AS total
FROM variant_assignments va
JOIN campaign_leads cl ON cl.id = va.campaign_lead_id
WHERE cl.campaign_id = $1
GROUP BY va.variant_id, va.step;
