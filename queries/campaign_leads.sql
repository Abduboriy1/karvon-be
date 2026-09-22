-- name: InsertCampaignLead :one
INSERT INTO campaign_leads (id, campaign_id, contact_id, business_id)
VALUES (sqlc.arg('id'), sqlc.arg('campaign_id'), sqlc.arg('contact_id'), sqlc.narg('business_id'))
ON CONFLICT (campaign_id, contact_id) DO NOTHING
RETURNING *;

-- name: GetCampaignLead :one
SELECT * FROM campaign_leads WHERE id = $1;

-- name: GetCampaignLeadByContact :one
SELECT * FROM campaign_leads WHERE campaign_id = $1 AND contact_id = $2;

-- name: GetCampaignLeadByInstantlyID :one
SELECT * FROM campaign_leads WHERE campaign_id = $1 AND instantly_lead_id = $2;

-- name: ListCampaignLeadsForContact :many
SELECT * FROM campaign_leads WHERE contact_id = $1 ORDER BY created_at;

-- name: ClaimPendingCampaignLeads :many
-- The push claim. A lead is claimed only while it is pending and its contact is not
-- suppressed, so a suppressed contact is never handed to the provider.
UPDATE campaign_leads cl
SET status = 'pushing', claimed_at = now(), push_attempts = push_attempts + 1, updated_at = now()
FROM contacts c
WHERE cl.id IN (
    SELECT l.id FROM campaign_leads l
    JOIN contacts ct ON ct.id = l.contact_id
    WHERE l.campaign_id = sqlc.arg('campaign_id') AND l.status = 'pending' AND ct.suppressed_at IS NULL
    ORDER BY l.created_at
    LIMIT sqlc.arg('lim')
    FOR UPDATE OF l SKIP LOCKED)
  AND c.id = cl.contact_id
RETURNING cl.*;

-- name: ReleaseCampaignLeadClaims :execrows
UPDATE campaign_leads
SET status = 'pending', claimed_at = NULL, last_push_error = sqlc.narg('error'), updated_at = now()
WHERE campaign_id = sqlc.arg('campaign_id') AND status = 'pushing' AND id = ANY(sqlc.arg('ids')::uuid[]);

-- name: MarkCampaignLeadPushed :one
UPDATE campaign_leads
SET status = 'active', instantly_lead_id = sqlc.narg('instantly_lead_id'), instantly_status = 1,
    pushed_at = COALESCE(pushed_at, now()), claimed_at = NULL, last_push_error = NULL,
    custom_vars = sqlc.arg('custom_vars'), updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: MarkCampaignLeadStatus :one
UPDATE campaign_leads
SET status = sqlc.arg('status'), last_push_error = sqlc.narg('error'), claimed_at = NULL, updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SetCampaignLeadProviderState :exec
UPDATE campaign_leads
SET instantly_lead_id = COALESCE(sqlc.narg('instantly_lead_id'), instantly_lead_id),
    instantly_status  = sqlc.narg('instantly_status'),
    interest_status   = sqlc.narg('interest_status'),
    interest_label    = sqlc.narg('interest_label'),
    status            = sqlc.arg('status'),
    open_count        = GREATEST(open_count, sqlc.arg('open_count')),
    click_count       = GREATEST(click_count, sqlc.arg('click_count')),
    reply_count       = GREATEST(reply_count, sqlc.arg('reply_count')),
    last_contacted_at = COALESCE(sqlc.narg('last_contacted_at'), last_contacted_at),
    last_opened_at    = COALESCE(sqlc.narg('last_opened_at'), last_opened_at),
    last_clicked_at   = COALESCE(sqlc.narg('last_clicked_at'), last_clicked_at),
    last_replied_at   = COALESCE(sqlc.narg('last_replied_at'), last_replied_at),
    updated_at        = now()
WHERE id = sqlc.arg('id');

-- name: RecordCampaignLeadContact :exec
UPDATE campaign_leads
SET status = CASE WHEN status IN ('pending', 'pushing', 'active') THEN 'active' ELSE status END,
    last_contacted_at = GREATEST(COALESCE(last_contacted_at, sqlc.arg('at')), sqlc.arg('at')),
    updated_at = now()
WHERE id = sqlc.arg('id');

-- name: RecordCampaignLeadOpen :exec
UPDATE campaign_leads
SET open_count = open_count + 1,
    last_opened_at = GREATEST(COALESCE(last_opened_at, sqlc.arg('at')), sqlc.arg('at')),
    updated_at = now()
WHERE id = sqlc.arg('id');

-- name: RecordCampaignLeadClick :exec
UPDATE campaign_leads
SET click_count = click_count + 1,
    last_clicked_at = GREATEST(COALESCE(last_clicked_at, sqlc.arg('at')), sqlc.arg('at')),
    updated_at = now()
WHERE id = sqlc.arg('id');

-- name: RecordCampaignLeadReply :exec
UPDATE campaign_leads
SET reply_count = reply_count + 1,
    status = CASE WHEN status IN ('pending', 'pushing', 'active', 'paused', 'completed') THEN 'replied' ELSE status END,
    last_replied_at = GREATEST(COALESCE(last_replied_at, sqlc.arg('at')), sqlc.arg('at')),
    updated_at = now()
WHERE id = sqlc.arg('id');

-- name: SetCampaignLeadInterest :exec
UPDATE campaign_leads
SET interest_status = sqlc.narg('interest_status'), interest_label = sqlc.narg('interest_label'), updated_at = now()
WHERE id = sqlc.arg('id');

-- name: MarkCampaignLeadTerminal :exec
UPDATE campaign_leads
SET status = sqlc.arg('status'), claimed_at = NULL, updated_at = now()
WHERE id = sqlc.arg('id') AND status NOT IN ('bounced', 'unsubscribed');

-- name: SuppressCampaignLeadsForContact :many
-- Every non-terminal lead of a suppressed contact stops where it is.
UPDATE campaign_leads
SET status = 'suppressed', claimed_at = NULL, updated_at = now()
WHERE contact_id = $1 AND status IN ('pending', 'pushing', 'active', 'paused', 'completed', 'replied')
RETURNING *;

-- name: DeleteCampaignLead :execrows
DELETE FROM campaign_leads WHERE id = $1 AND status IN ('pending', 'failed', 'skipped');

-- name: CountCampaignLeadsByStatus :many
SELECT status, count(*)::bigint AS total FROM campaign_leads WHERE campaign_id = $1 GROUP BY status;

-- name: CountPendingCampaignLeads :one
SELECT count(*) FROM campaign_leads cl
JOIN contacts c ON c.id = cl.contact_id
WHERE cl.campaign_id = $1 AND cl.status = 'pending' AND c.suppressed_at IS NULL;

-- name: ListPushedCampaignLeads :many
SELECT * FROM campaign_leads WHERE campaign_id = $1 AND instantly_lead_id IS NOT NULL ORDER BY created_at;

-- name: ListCampaignLeadContactIDs :many
SELECT contact_id FROM campaign_leads WHERE campaign_id = $1;

-- name: ListPushedCampaignLeadsForContact :many
-- Every lead of a contact that reached the provider, whatever state it is in now.
-- Suppression uses this rather than the rows it just stopped, because a lead can
-- already be terminal (bounced, unsubscribed) and still be sitting in Instantly.
SELECT cl.* FROM campaign_leads cl
JOIN campaigns c ON c.id = cl.campaign_id
WHERE cl.contact_id = $1
  AND cl.instantly_lead_id IS NOT NULL
  AND c.status <> 'archived';
