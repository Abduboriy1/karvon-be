-- +goose Up
-- Two campaign features that share one migration.
--
-- 1. Scheduled launch. A campaign can be told to launch at a future moment: it
--    waits in the new 'scheduled' status and the launch job runs at
--    scheduled_launch_at. launch_request_id names the launch request the current
--    job belongs to, so a job left over from an earlier request (rescheduled,
--    unscheduled, or launched now instead) recognises itself as stale and stops.
--
-- 2. Instantly cleanup. Instantly's plans cap "uploaded contacts" (every lead
--    sitting in a campaign, whatever its status), and deleting a lead from its
--    campaign frees the slot. A cleanup run deletes leads Instantly has finished
--    with, while Karvon keeps the lead row, its sends and its timeline: the lead is
--    stamped provider_removed_at instead of being forgotten. Removals made for a
--    suppression or an exclusion are stamped the same way, with their own reason.
-- +goose StatementBegin
ALTER TABLE campaigns DROP CONSTRAINT IF EXISTS campaigns_status_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaigns ADD CONSTRAINT campaigns_status_check
    CHECK (status IN ('draft', 'ready', 'scheduled', 'launching', 'active', 'paused',
                      'completed', 'failed', 'archived'));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaigns
    ADD COLUMN scheduled_launch_at timestamptz,
    ADD COLUMN launch_request_id   uuid;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX campaigns_scheduled_idx ON campaigns (scheduled_launch_at) WHERE status = 'scheduled';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE instantly_cleanup_runs (
    id              uuid PRIMARY KEY,
    trigger         text NOT NULL CHECK (trigger IN ('manual', 'auto')),
    status          text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'failed')),
    scope           text NOT NULL CHECK (scope IN ('finished', 'emailed')),
    min_idle_days   integer NOT NULL CHECK (min_idle_days BETWEEN 0 AND 90),
    include_replied boolean NOT NULL DEFAULT false,
    campaign_ids    uuid[] NOT NULL DEFAULT '{}',
    max_leads       integer CHECK (max_leads IS NULL OR max_leads > 0),
    selected        integer NOT NULL DEFAULT 0,
    removed         integer NOT NULL DEFAULT 0,
    already_gone    integer NOT NULL DEFAULT 0,
    failed          integer NOT NULL DEFAULT 0,
    error           text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,
    finished_at     timestamptz
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX instantly_cleanup_runs_created_idx ON instantly_cleanup_runs (created_at DESC);
-- +goose StatementEnd
-- At most one run is in flight: two would race to delete the same leads.
-- +goose StatementBegin
CREATE UNIQUE INDEX instantly_cleanup_runs_active_idx ON instantly_cleanup_runs ((true))
    WHERE status IN ('queued', 'running');
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE campaign_leads
    ADD COLUMN provider_removed_at     timestamptz,
    ADD COLUMN provider_removed_reason text
        CHECK (provider_removed_reason IN ('cleanup', 'suppressed', 'excluded', 'manual')),
    ADD COLUMN cleanup_run_id          uuid REFERENCES instantly_cleanup_runs (id) ON DELETE SET NULL,
    ADD CONSTRAINT campaign_leads_provider_removed_check
        CHECK ((provider_removed_at IS NULL) = (provider_removed_reason IS NULL));
-- +goose StatementEnd
-- The leads still occupying an Instantly slot: what the capacity estimate counts
-- and what a cleanup scans.
-- +goose StatementBegin
CREATE INDEX campaign_leads_in_provider_idx ON campaign_leads (campaign_id)
    WHERE instantly_lead_id IS NOT NULL AND provider_removed_at IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE campaign_settings
    ADD COLUMN instantly_contact_limit integer CHECK (instantly_contact_limit IS NULL OR instantly_contact_limit > 0),
    ADD COLUMN cleanup_auto_enabled    boolean NOT NULL DEFAULT false,
    ADD COLUMN cleanup_scope           text NOT NULL DEFAULT 'finished' CHECK (cleanup_scope IN ('finished', 'emailed')),
    ADD COLUMN cleanup_min_idle_days   integer NOT NULL DEFAULT 3 CHECK (cleanup_min_idle_days BETWEEN 0 AND 90),
    ADD COLUMN cleanup_include_replied boolean NOT NULL DEFAULT false;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE contact_events DROP CONSTRAINT IF EXISTS contact_events_type_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE contact_events ADD CONSTRAINT contact_events_type_check CHECK (type IN (
    'imported', 'queued', 'pushed', 'push_failed', 'sent', 'opened', 'clicked',
    'replied', 'auto_replied', 'interested', 'not_interested', 'wrong_person',
    'meeting_booked', 'bounced', 'unsubscribed', 'skipped', 'suppressed',
    'suppression_lifted', 'permission_requested', 'consent_captured',
    'consent_revoked', 'newsletter_eligible', 'newsletter_pushed',
    'newsletter_pending', 'newsletter_subscribed', 'newsletter_unsubscribed',
    'newsletter_cleaned', 'newsletter_profile_updated', 'stage_changed', 'note',
    'removed_from_provider'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM contact_events WHERE type = 'removed_from_provider';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE contact_events DROP CONSTRAINT IF EXISTS contact_events_type_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE contact_events ADD CONSTRAINT contact_events_type_check CHECK (type IN (
    'imported', 'queued', 'pushed', 'push_failed', 'sent', 'opened', 'clicked',
    'replied', 'auto_replied', 'interested', 'not_interested', 'wrong_person',
    'meeting_booked', 'bounced', 'unsubscribed', 'skipped', 'suppressed',
    'suppression_lifted', 'permission_requested', 'consent_captured',
    'consent_revoked', 'newsletter_eligible', 'newsletter_pushed',
    'newsletter_pending', 'newsletter_subscribed', 'newsletter_unsubscribed',
    'newsletter_cleaned', 'newsletter_profile_updated', 'stage_changed', 'note'));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaign_settings
    DROP COLUMN IF EXISTS instantly_contact_limit,
    DROP COLUMN IF EXISTS cleanup_auto_enabled,
    DROP COLUMN IF EXISTS cleanup_scope,
    DROP COLUMN IF EXISTS cleanup_min_idle_days,
    DROP COLUMN IF EXISTS cleanup_include_replied;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS campaign_leads_in_provider_idx;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaign_leads
    DROP CONSTRAINT IF EXISTS campaign_leads_provider_removed_check,
    DROP COLUMN IF EXISTS cleanup_run_id,
    DROP COLUMN IF EXISTS provider_removed_reason,
    DROP COLUMN IF EXISTS provider_removed_at;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS instantly_cleanup_runs;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE campaigns SET status = 'draft' WHERE status = 'scheduled';
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS campaigns_scheduled_idx;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaigns DROP COLUMN IF EXISTS launch_request_id, DROP COLUMN IF EXISTS scheduled_launch_at;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaigns DROP CONSTRAINT IF EXISTS campaigns_status_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaigns ADD CONSTRAINT campaigns_status_check
    CHECK (status IN ('draft', 'ready', 'launching', 'active', 'paused', 'completed', 'failed', 'archived'));
-- +goose StatementEnd
