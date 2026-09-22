-- name: ListCampaignVariants :many
SELECT cv.campaign_id, cv.variant_id, cv.step, cv.weight, cv.status AS assignment_status, cv.created_at AS attached_at,
       v.name, v.status AS variant_status, v.subject_template, v.body_template, v.component_ids
FROM campaign_variants cv
JOIN email_variants v ON v.id = cv.variant_id
WHERE cv.campaign_id = $1
ORDER BY cv.step, v.name;

-- name: ListActiveCampaignVariantsForStep :many
SELECT cv.variant_id, cv.weight FROM campaign_variants cv
JOIN email_variants v ON v.id = cv.variant_id
WHERE cv.campaign_id = $1 AND cv.step = $2 AND cv.status = 'active' AND cv.weight > 0
  AND v.status IN ('approved', 'active')
ORDER BY cv.variant_id;

-- name: ClearCampaignVariants :exec
DELETE FROM campaign_variants WHERE campaign_id = $1;

-- name: AddCampaignVariant :exec
INSERT INTO campaign_variants (campaign_id, variant_id, step, weight, status)
VALUES (sqlc.arg('campaign_id'), sqlc.arg('variant_id'), sqlc.arg('step'), sqlc.arg('weight'), sqlc.arg('status'));

-- name: ActivateCampaignVariants :exec
-- A launched campaign's variants become active content.
UPDATE email_variants SET status = 'active', updated_at = now()
WHERE id IN (SELECT variant_id FROM campaign_variants WHERE campaign_id = $1) AND status = 'approved';

-- name: ListCampaignsUsingVariant :many
SELECT c.* FROM campaigns c JOIN campaign_variants cv ON cv.campaign_id = c.id WHERE cv.variant_id = $1 ORDER BY c.created_at DESC;
