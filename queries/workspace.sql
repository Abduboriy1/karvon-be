-- name: GetWorkspaceSettings :one
SELECT * FROM workspace_settings WHERE id = 1;

-- name: SetWorkspaceAdminEmail :one
UPDATE workspace_settings
SET admin_email = sqlc.narg('admin_email'),
    updated_at  = now()
WHERE id = 1
RETURNING *;

-- name: SetWorkspaceServiceAccount :one
UPDATE workspace_settings
SET service_account_email     = sqlc.narg('service_account_email'),
    service_account_client_id = sqlc.narg('service_account_client_id'),
    updated_at                = now()
WHERE id = 1
RETURNING *;

-- name: CreateWorkspaceDomain :one
INSERT INTO workspace_domains (id, domain_name)
VALUES ($1, $2)
RETURNING *;

-- name: CreateWorkspaceMailbox :one
INSERT INTO workspace_mailboxes (id, domain_id, position, email, given_name, family_name, password_enc)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetWorkspaceDomain :one
SELECT * FROM workspace_domains WHERE id = $1;

-- name: GetWorkspaceDomainByName :one
SELECT * FROM workspace_domains WHERE domain_name = $1;

-- name: ListWorkspaceDomains :many
SELECT * FROM workspace_domains
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountWorkspaceDomains :one
SELECT count(*) FROM workspace_domains;

-- name: ListWorkspaceMailboxes :many
SELECT * FROM workspace_mailboxes WHERE domain_id = $1 ORDER BY position;

-- name: ListWorkspaceMailboxesFor :many
SELECT * FROM workspace_mailboxes
WHERE domain_id = ANY(sqlc.arg('domain_ids')::uuid[])
ORDER BY domain_id, position;

-- name: GetWorkspaceMailbox :one
SELECT * FROM workspace_mailboxes WHERE id = $1;

-- name: MarkWorkspaceDomainAdded :exec
UPDATE workspace_domains
SET added_at = COALESCE(added_at, now()), updated_at = now()
WHERE id = $1;

-- name: SetWorkspaceVerificationToken :exec
UPDATE workspace_domains
SET verification_token = $2, updated_at = now()
WHERE id = $1;

-- name: MarkWorkspaceDNSPublished :exec
UPDATE workspace_domains
SET dns_published_at = COALESCE(dns_published_at, now()), updated_at = now()
WHERE id = $1;

-- name: MarkWorkspaceDomainVerified :exec
UPDATE workspace_domains
SET verified_at = COALESCE(verified_at, now()), updated_at = now()
WHERE id = $1;

-- The automatic steps are done. A domain whose DKIM record was already published (a
-- retry after adding a mailbox failed) goes straight back to active.
-- name: FinishWorkspaceProvisioning :one
UPDATE workspace_domains
SET status        = CASE WHEN dkim_published_at IS NULL THEN 'dkim_required' ELSE 'active' END,
    error_code    = NULL,
    error_message = NULL,
    updated_at    = now()
WHERE id = $1 AND status = 'provisioning'
RETURNING *;

-- name: FailWorkspaceDomain :exec
UPDATE workspace_domains
SET status        = 'failed',
    error_code    = sqlc.arg('error_code'),
    error_message = sqlc.arg('error_message'),
    updated_at    = now()
WHERE id = sqlc.arg('id') AND status = 'provisioning';

-- name: RetryWorkspaceDomain :one
UPDATE workspace_domains
SET status = 'provisioning', error_code = NULL, error_message = NULL, updated_at = now()
WHERE id = $1 AND status = 'failed'
RETURNING *;

-- name: PublishWorkspaceDKIM :one
UPDATE workspace_domains
SET status            = 'active',
    dkim_selector     = sqlc.arg('dkim_selector'),
    dkim_published_at = now(),
    updated_at        = now()
WHERE id = sqlc.arg('id') AND status IN ('dkim_required', 'active')
RETURNING *;

-- The attempt is counted before the billable call, so the next pass knows whether a
-- "this address already exists" answer can be its own earlier call landing.
-- name: CountWorkspaceMailboxAttempt :one
UPDATE workspace_mailboxes
SET create_attempts = create_attempts + 1, updated_at = now()
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- An attempt Google refused without looking at it (throttled, or the credentials
-- were refused) does not count.
-- name: UncountWorkspaceMailboxAttempt :exec
UPDATE workspace_mailboxes
SET create_attempts = greatest(create_attempts - 1, 0), updated_at = now()
WHERE id = $1 AND status = 'pending';

-- name: SucceedWorkspaceMailbox :exec
UPDATE workspace_mailboxes
SET status         = 'created',
    google_user_id = sqlc.narg('google_user_id'),
    provisioned_at = now(),
    error_code     = NULL,
    error_message  = NULL,
    updated_at     = now()
WHERE id = sqlc.arg('id') AND status = 'pending';

-- name: FailWorkspaceMailbox :exec
UPDATE workspace_mailboxes
SET status        = 'failed',
    error_code    = sqlc.arg('error_code'),
    error_message = sqlc.arg('error_message'),
    updated_at    = now()
WHERE id = sqlc.arg('id') AND status = 'pending';

-- A retry sends refused mailboxes again. An address that belonged to someone else is
-- left failed: sending it again would count it as ours.
-- name: ResetFailedWorkspaceMailboxes :exec
UPDATE workspace_mailboxes
SET status = 'pending', create_attempts = 0, error_code = NULL, error_message = NULL, updated_at = now()
WHERE domain_id = $1 AND status = 'failed' AND error_code IS DISTINCT FROM 'address_taken';

-- A domain that is set up takes more mailboxes by going back to provisioning; the
-- worker skips every finished step and creates the new ones.
-- name: ReopenWorkspaceDomain :one
UPDATE workspace_domains
SET status = 'provisioning', error_code = NULL, error_message = NULL, updated_at = now()
WHERE id = $1 AND status IN ('dkim_required', 'active')
RETURNING *;

-- Only a mailbox that was never created is just a record; removing it frees its slot.
-- name: DeleteFailedWorkspaceMailbox :one
DELETE FROM workspace_mailboxes
WHERE id = $1 AND status = 'failed'
RETURNING *;

-- name: StartInstantlyConnection :one
UPDATE workspace_mailboxes
SET instantly_status             = 'connecting',
    instantly_session_id         = sqlc.arg('session_id'),
    instantly_auth_url           = sqlc.arg('auth_url'),
    instantly_session_expires_at = sqlc.arg('expires_at'),
    instantly_warmup             = sqlc.arg('warmup'),
    instantly_error              = NULL,
    updated_at                   = now()
WHERE id = sqlc.arg('id') AND status = 'created'
  AND instantly_status IS DISTINCT FROM 'connected'
RETURNING *;

-- The session id guards every outcome, so a session the operator has since replaced
-- cannot overwrite the new one.
-- name: ConnectInstantly :one
UPDATE workspace_mailboxes
SET instantly_status       = 'connected',
    instantly_account_id   = sqlc.narg('account_id'),
    instantly_connected_at = now(),
    instantly_error        = NULL,
    instantly_auth_url     = NULL,
    updated_at             = now()
WHERE id = sqlc.arg('id') AND instantly_session_id = sqlc.arg('session_id') AND instantly_status = 'connecting'
RETURNING *;

-- name: EndInstantlyConnection :exec
UPDATE workspace_mailboxes
SET instantly_status   = sqlc.arg('status'),
    instantly_error    = sqlc.narg('error'),
    instantly_auth_url = NULL,
    updated_at         = now()
WHERE id = sqlc.arg('id') AND instantly_session_id = sqlc.arg('session_id') AND instantly_status = 'connecting';

-- name: SetInstantlyWarmupResult :exec
UPDATE workspace_mailboxes
SET instantly_warmup_enabled_at = sqlc.narg('enabled_at'),
    instantly_error             = sqlc.narg('error'),
    updated_at                  = now()
WHERE id = sqlc.arg('id') AND instantly_status = 'connected';
