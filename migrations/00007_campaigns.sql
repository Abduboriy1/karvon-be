-- +goose Up

-- Campaign module.
--
-- Two email stages share one lead lifecycle. Stage one is cold outreach, sent by
-- Instantly; stage two is permission-based newsletter marketing, sent by Mailchimp.
-- This database is the source of truth for the campaign relationships, the lead
-- lifecycle, the content components and variants, the per-lead variant assignment,
-- the normalised event log, the consent and suppression state, and every provider
-- id. Instantly and Mailchimp own delivery; nothing here sends an email.
--
-- Every enum is text plus a CHECK constraint mirrored by Go constants
-- (internal/campaign/types.go); tests/campaign_schema_test.go keeps the two in step.

-- Provider credentials reuse the sources table: one encrypted-key path, one Sources
-- page, one connection test.
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_role_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_role_check
    CHECK (role IN ('maps', 'verifier', 'outreach', 'newsletter'));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_kind_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_kind_check
    CHECK (kind IN ('apify', 'outscraper', 'emailable', 'instantly', 'mailchimp'));
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO sources (id, kind, name, role, cost_per_1k_cents, enabled)
VALUES
    ('0192f000-0000-7000-8000-000000000004', 'instantly', 'Instantly · Cold outreach', 'outreach', 0, false),
    ('0192f000-0000-7000-8000-000000000005', 'mailchimp', 'Mailchimp · Newsletter', 'newsletter', 0, false)
ON CONFLICT (kind) DO NOTHING;
-- +goose StatementEnd

-- One row of integration state that is not a credential: the Instantly webhook we
-- registered, the path token it posts to, and the secret header we asked it to send.
-- +goose StatementBegin
CREATE TABLE campaign_settings (
    id                           smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    instantly_webhook_token      text UNIQUE,
    instantly_webhook_secret_enc bytea,
    instantly_webhook_id         text,
    instantly_webhook_url        text,
    instantly_webhook_status     integer,
    instantly_webhook_error      text,
    default_audience_id          uuid,
    updated_at                   timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO campaign_settings (id) VALUES (1) ON CONFLICT DO NOTHING;
-- +goose StatementEnd

-- A contact is one person, keyed by address, across every campaign. The lifecycle
-- stage is the funnel position; it only ever moves forward, a terminal stage beats any
-- funnel stage, and unsubscribed beats everything.
-- +goose StatementBegin
CREATE TABLE contacts (
    id                 uuid PRIMARY KEY,
    email              citext NOT NULL UNIQUE,
    domain             text NOT NULL,
    first_name         text,
    last_name          text,
    company            text,
    title              text,
    phone              text,
    website            text,
    business_id        uuid REFERENCES businesses (id) ON DELETE SET NULL,
    source             text NOT NULL DEFAULT 'business_import'
                       CHECK (source IN ('business_import', 'csv', 'manual')),
    lifecycle_stage    text NOT NULL DEFAULT 'cold'
                       CHECK (lifecycle_stage IN (
                           'cold', 'queued_for_instantly', 'contacted', 'engaged', 'replied',
                           'interested', 'permission_requested', 'permission_captured',
                           'newsletter_eligible', 'mailchimp_pending', 'mailchimp_subscribed',
                           'not_interested', 'wrong_person', 'do_not_contact',
                           'invalid_email', 'bounced', 'unsubscribed')),
    stage_changed_at   timestamptz NOT NULL DEFAULT now(),
    suppressed_at      timestamptz,
    suppression_reason text CHECK (suppression_reason IN (
                           'unsubscribed', 'bounced', 'do_not_contact', 'invalid_email',
                           'not_interested', 'wrong_person')),
    attributes         jsonb NOT NULL DEFAULT '{}'::jsonb,
    last_event_at      timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK ((suppressed_at IS NULL) = (suppression_reason IS NULL))
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contacts_stage_idx ON contacts (lifecycle_stage);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contacts_business_idx ON contacts (business_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contacts_created_idx ON contacts (created_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contacts_email_trgm_idx ON contacts USING gin ((email::text) gin_trgm_ops);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contacts_pushable_idx ON contacts (id) WHERE suppressed_at IS NULL;
-- +goose StatementEnd

-- Consent is a record, not a flag: who said yes, how, when, and what the evidence was.
-- A contact has at most one active consent; revoking it keeps the history.
-- +goose StatementBegin
CREATE TABLE contact_consents (
    id                uuid PRIMARY KEY,
    contact_id        uuid NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    source            text NOT NULL CHECK (source IN ('explicit_reply', 'form', 'verbal_confirmed')),
    captured_at       timestamptz NOT NULL,
    evidence          text NOT NULL CHECK (length(evidence) BETWEEN 1 AND 4000),
    captured_by       text NOT NULL,
    campaign_lead_id  uuid,
    provider_event_id uuid,
    revoked_at        timestamptz,
    revoke_reason     text,
    created_at        timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX contact_consents_active_idx ON contact_consents (contact_id) WHERE revoked_at IS NULL;
-- +goose StatementEnd

-- Suppression history is append-only. Unsubscribed, bounced and invalid are
-- permanent; the other reasons can be lifted with a note.
-- +goose StatementBegin
CREATE TABLE contact_suppressions (
    id                uuid PRIMARY KEY,
    contact_id        uuid NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    reason            text NOT NULL CHECK (reason IN (
                          'unsubscribed', 'bounced', 'do_not_contact', 'invalid_email',
                          'not_interested', 'wrong_person')),
    source            text NOT NULL CHECK (source IN (
                          'instantly_webhook', 'mailchimp_webhook', 'reconcile', 'manual', 'import')),
    note              text,
    campaign_lead_id  uuid,
    provider_event_id uuid,
    created_at        timestamptz NOT NULL DEFAULT now(),
    lifted_at         timestamptz,
    lifted_note       text,
    CHECK (reason NOT IN ('unsubscribed', 'bounced', 'invalid_email') OR lifted_at IS NULL)
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX contact_suppressions_active_idx ON contact_suppressions (contact_id) WHERE lifted_at IS NULL;
-- +goose StatementEnd

-- The guard on contacts: an unsubscribe can never be undone, a newsletter stage
-- needs an active consent, and a suppressed contact always sits in a terminal stage.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION contacts_guard_stage() RETURNS trigger AS $$
BEGIN
    IF OLD.lifecycle_stage = 'unsubscribed' AND NEW.lifecycle_stage <> 'unsubscribed' THEN
        RAISE EXCEPTION 'contacts: % is unsubscribed and can never become active again', OLD.email
            USING ERRCODE = 'raise_exception';
    END IF;
    IF OLD.suppression_reason = 'unsubscribed' AND NEW.suppressed_at IS NULL THEN
        RAISE EXCEPTION 'contacts: the unsubscribe on % may not be lifted', OLD.email
            USING ERRCODE = 'raise_exception';
    END IF;
    IF NEW.lifecycle_stage IN ('newsletter_eligible', 'mailchimp_pending', 'mailchimp_subscribed')
       AND NOT EXISTS (SELECT 1 FROM contact_consents c WHERE c.contact_id = NEW.id AND c.revoked_at IS NULL) THEN
        RAISE EXCEPTION 'contacts: % cannot enter % without an active consent record', OLD.email, NEW.lifecycle_stage
            USING ERRCODE = 'raise_exception';
    END IF;
    IF NEW.suppressed_at IS NOT NULL AND NEW.lifecycle_stage NOT IN (
        'not_interested', 'wrong_person', 'do_not_contact', 'invalid_email', 'bounced', 'unsubscribed') THEN
        RAISE EXCEPTION 'contacts: % is suppressed and must stay in a terminal stage', OLD.email
            USING ERRCODE = 'raise_exception';
    END IF;
    IF NEW.lifecycle_stage IS DISTINCT FROM OLD.lifecycle_stage THEN
        NEW.stage_changed_at := now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER contacts_guard_stage
    BEFORE UPDATE ON contacts
    FOR EACH ROW EXECUTE FUNCTION contacts_guard_stage();
-- +goose StatementEnd

-- Instantly's sending accounts, mirrored read-only. Instantly keys them by address.
-- +goose StatementBegin
CREATE TABLE sending_accounts (
    id                     uuid PRIMARY KEY,
    email                  citext NOT NULL UNIQUE,
    first_name             text,
    last_name              text,
    provider_code          integer,
    status                 integer NOT NULL DEFAULT 0,
    warmup_status          integer,
    daily_limit            integer,
    sending_gap            integer,
    warmup_score           integer,
    status_message         text,
    tracking_domain        text,
    tracking_domain_status text,
    setup_pending          boolean NOT NULL DEFAULT false,
    is_managed             boolean NOT NULL DEFAULT false,
    raw                    jsonb NOT NULL DEFAULT '{}'::jsonb,
    last_synced_at         timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE campaigns (
    id                           uuid PRIMARY KEY,
    name                         text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    status                       text NOT NULL DEFAULT 'draft'
                                 CHECK (status IN ('draft', 'ready', 'launching', 'active', 'paused',
                                                   'completed', 'failed', 'archived')),
    brief                        jsonb NOT NULL DEFAULT '{}'::jsonb,
    schedule                     jsonb NOT NULL DEFAULT '{}'::jsonb,
    settings                     jsonb NOT NULL DEFAULT '{}'::jsonb,
    steps                        integer NOT NULL DEFAULT 1 CHECK (steps BETWEEN 1 AND 5),
    step_delays                  jsonb NOT NULL DEFAULT '[]'::jsonb,
    weights_version              integer NOT NULL DEFAULT 1,
    instantly_campaign_id        text UNIQUE,
    instantly_status             integer,
    instantly_sending_status     text,
    instantly_not_sending_status integer,
    launch_claimed_at            timestamptz,
    launched_at                  timestamptz,
    paused_at                    timestamptz,
    completed_at                 timestamptz,
    archived_at                  timestamptz,
    last_synced_at               timestamptz,
    last_sync_error              text,
    error                        text,
    leads_total                  integer NOT NULL DEFAULT 0,
    leads_pushed                 integer NOT NULL DEFAULT 0,
    created_at                   timestamptz NOT NULL DEFAULT now(),
    updated_at                   timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX campaigns_status_idx ON campaigns (status);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX campaigns_created_idx ON campaigns (created_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX campaigns_name_trgm_idx ON campaigns USING gin (name gin_trgm_ops);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE campaign_sending_accounts (
    campaign_id        uuid NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    sending_account_id uuid NOT NULL REFERENCES sending_accounts (id) ON DELETE RESTRICT,
    PRIMARY KEY (campaign_id, sending_account_id)
);
-- +goose StatementEnd

-- A campaign lead is one contact inside one campaign. Its status is the delivery
-- state; the contact's lifecycle stage is the funnel position across campaigns.
-- +goose StatementBegin
CREATE TABLE campaign_leads (
    id                uuid PRIMARY KEY,
    campaign_id       uuid NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    contact_id        uuid NOT NULL REFERENCES contacts (id) ON DELETE RESTRICT,
    business_id       uuid REFERENCES businesses (id) ON DELETE SET NULL,
    status            text NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'pushing', 'active', 'paused', 'completed', 'replied',
                                        'bounced', 'unsubscribed', 'skipped', 'suppressed', 'failed')),
    instantly_lead_id text,
    instantly_status  integer,
    interest_status   integer,
    interest_label    text,
    claimed_at        timestamptz,
    pushed_at         timestamptz,
    push_attempts     integer NOT NULL DEFAULT 0,
    last_push_error   text,
    custom_vars       jsonb NOT NULL DEFAULT '{}'::jsonb,
    last_contacted_at timestamptz,
    last_opened_at    timestamptz,
    last_clicked_at   timestamptz,
    last_replied_at   timestamptz,
    open_count        integer NOT NULL DEFAULT 0,
    click_count       integer NOT NULL DEFAULT 0,
    reply_count       integer NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (campaign_id, contact_id),
    UNIQUE (campaign_id, instantly_lead_id)
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX campaign_leads_status_idx ON campaign_leads (campaign_id, status);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX campaign_leads_contact_idx ON campaign_leads (contact_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX campaign_leads_pending_idx ON campaign_leads (campaign_id, created_at) WHERE status = 'pending';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_campaign_lead_fk
    FOREIGN KEY (campaign_lead_id) REFERENCES campaign_leads (id) ON DELETE SET NULL;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE contact_suppressions ADD CONSTRAINT contact_suppressions_campaign_lead_fk
    FOREIGN KEY (campaign_lead_id) REFERENCES campaign_leads (id) ON DELETE SET NULL;
-- +goose StatementEnd

-- AI generation records: the brief, the prompt that was built from it, the raw text
-- pasted back (or returned by the API), and what was imported from it.
-- +goose StatementBegin
CREATE TABLE ai_generations (
    id                     uuid PRIMARY KEY,
    provider               text NOT NULL CHECK (provider IN ('manual_chatgpt', 'openai_api')),
    model                  text,
    status                 text NOT NULL DEFAULT 'prompt_built'
                           CHECK (status IN ('prompt_built', 'awaiting_paste', 'parsed', 'imported', 'failed')),
    campaign_id            uuid REFERENCES campaigns (id) ON DELETE SET NULL,
    brief                  jsonb NOT NULL DEFAULT '{}'::jsonb,
    prompt                 text NOT NULL,
    prompt_version         text NOT NULL,
    raw_output             text,
    parsed                 jsonb,
    component_count        integer NOT NULL DEFAULT 0,
    variant_count          integer NOT NULL DEFAULT 0,
    imported_component_ids uuid[] NOT NULL DEFAULT '{}',
    imported_variant_ids   uuid[] NOT NULL DEFAULT '{}',
    usage                  jsonb,
    error                  text,
    created_at             timestamptz NOT NULL DEFAULT now(),
    parsed_at              timestamptz,
    imported_at            timestamptz
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX ai_generations_created_idx ON ai_generations (created_at DESC);
-- +goose StatementEnd

-- Reusable email building blocks. A component's body is what a variant assembles;
-- once a variant that uses it has been sent, the body is frozen (create a new one).
-- +goose StatementBegin
CREATE TABLE email_components (
    id               uuid PRIMARY KEY,
    type             text NOT NULL CHECK (type IN (
                         'subject', 'hook', 'problem', 'value_prop', 'proof', 'cta', 'closing', 'ps')),
    name             text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    body             text NOT NULL CHECK (length(body) BETWEEN 1 AND 5000),
    status           text NOT NULL DEFAULT 'draft'
                     CHECK (status IN ('draft', 'ai_generated', 'reviewed', 'approved', 'active', 'archived')),
    tags             text[] NOT NULL DEFAULT '{}',
    language         text NOT NULL DEFAULT 'en',
    ai_generation_id uuid REFERENCES ai_generations (id) ON DELETE SET NULL,
    placeholders     text[] NOT NULL DEFAULT '{}',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    archived_at      timestamptz
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_components_type_status_idx ON email_components (type, status);
-- +goose StatementEnd

-- An assembled email: an ordered set of components rendered into a subject and a
-- body template. The templates are stored so a variant is reproducible even after a
-- component is archived.
-- +goose StatementBegin
CREATE TABLE email_variants (
    id               uuid PRIMARY KEY,
    name             text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    step             integer NOT NULL DEFAULT 1 CHECK (step BETWEEN 1 AND 5),
    status           text NOT NULL DEFAULT 'draft'
                     CHECK (status IN ('draft', 'ai_generated', 'reviewed', 'approved', 'active', 'archived')),
    subject_template text NOT NULL,
    body_template    text NOT NULL,
    component_ids    uuid[] NOT NULL DEFAULT '{}',
    ai_generation_id uuid REFERENCES ai_generations (id) ON DELETE SET NULL,
    notes            text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    archived_at      timestamptz
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_variants_status_idx ON email_variants (status, step);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE email_variant_components (
    variant_id   uuid NOT NULL REFERENCES email_variants (id) ON DELETE CASCADE,
    component_id uuid NOT NULL REFERENCES email_components (id) ON DELETE RESTRICT,
    position     integer NOT NULL CHECK (position >= 0),
    slot         text NOT NULL CHECK (slot IN (
                     'subject', 'hook', 'problem', 'value_prop', 'proof', 'cta', 'closing', 'ps')),
    PRIMARY KEY (variant_id, position),
    UNIQUE (variant_id, slot)
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_variant_components_component_idx ON email_variant_components (component_id);
-- +goose StatementEnd

-- Which variants a campaign rotates, with weights. Weights per (campaign, step) total
-- 100; the service enforces it and bumps campaigns.weights_version on every change.
-- +goose StatementBegin
CREATE TABLE campaign_variants (
    campaign_id uuid NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    variant_id  uuid NOT NULL REFERENCES email_variants (id) ON DELETE RESTRICT,
    step        integer NOT NULL CHECK (step BETWEEN 1 AND 5),
    weight      integer NOT NULL CHECK (weight BETWEEN 0 AND 100),
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (campaign_id, variant_id)
);
-- +goose StatementEnd

-- The variant a lead was given for a step, with the exact subject and body it was
-- rendered into. Once locked (pushed to the provider) the assignment is immutable, so
-- every later event stays attributed to what was actually sent.
-- +goose StatementBegin
CREATE TABLE variant_assignments (
    id                   uuid PRIMARY KEY,
    campaign_lead_id     uuid NOT NULL REFERENCES campaign_leads (id) ON DELETE CASCADE,
    step                 integer NOT NULL CHECK (step BETWEEN 1 AND 5),
    variant_id           uuid NOT NULL REFERENCES email_variants (id) ON DELETE RESTRICT,
    weights_version      integer NOT NULL,
    seed_hash            text NOT NULL,
    rendered_subject     text NOT NULL,
    rendered_body        text NOT NULL,
    component_ids        uuid[] NOT NULL DEFAULT '{}',
    subject_component_id uuid,
    hook_component_id    uuid,
    cta_component_id     uuid,
    assigned_at          timestamptz NOT NULL DEFAULT now(),
    locked_at            timestamptz,
    UNIQUE (campaign_lead_id, step)
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX variant_assignments_variant_idx ON variant_assignments (variant_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION variant_assignments_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.locked_at IS NOT NULL THEN
            RAISE EXCEPTION 'variant_assignments: assignment % is locked and may not be deleted', OLD.id
                USING ERRCODE = 'raise_exception';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.locked_at IS NOT NULL AND (
        NEW.variant_id IS DISTINCT FROM OLD.variant_id
        OR NEW.rendered_subject IS DISTINCT FROM OLD.rendered_subject
        OR NEW.rendered_body IS DISTINCT FROM OLD.rendered_body
        OR NEW.component_ids IS DISTINCT FROM OLD.component_ids
        OR NEW.locked_at IS DISTINCT FROM OLD.locked_at
        OR NEW.step IS DISTINCT FROM OLD.step) THEN
        RAISE EXCEPTION 'variant_assignments: assignment % is locked; what was sent may not be rewritten', OLD.id
            USING ERRCODE = 'raise_exception';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER variant_assignments_guard
    BEFORE UPDATE OR DELETE ON variant_assignments
    FOR EACH ROW EXECUTE FUNCTION variant_assignments_guard();
-- +goose StatementEnd

-- One row per email the provider sent to a lead for a step, with everything an
-- analytics question needs denormalised onto it.
-- +goose StatementBegin
CREATE TABLE email_sends (
    id                    uuid PRIMARY KEY,
    campaign_lead_id      uuid NOT NULL REFERENCES campaign_leads (id) ON DELETE CASCADE,
    assignment_id         uuid REFERENCES variant_assignments (id) ON DELETE SET NULL,
    campaign_id           uuid NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    contact_id            uuid NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    step                  integer NOT NULL CHECK (step BETWEEN 1 AND 5),
    variant_id            uuid REFERENCES email_variants (id) ON DELETE SET NULL,
    sending_account_email citext,
    sending_account_id    uuid REFERENCES sending_accounts (id) ON DELETE SET NULL,
    instantly_email_id    text,
    provider_message_id   text,
    subject_snapshot      text,
    sent_at               timestamptz NOT NULL,
    first_opened_at       timestamptz,
    last_opened_at        timestamptz,
    open_count            integer NOT NULL DEFAULT 0,
    first_clicked_at      timestamptz,
    last_clicked_at       timestamptz,
    click_count           integer NOT NULL DEFAULT 0,
    replied_at            timestamptz,
    reply_classification  text CHECK (reply_classification IN (
                              'positive', 'negative', 'neutral', 'auto_reply', 'out_of_office', 'unknown')),
    bounced_at            timestamptz,
    unsubscribed_at       timestamptz,
    source                text NOT NULL CHECK (source IN ('webhook', 'reconcile')),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (campaign_lead_id, step)
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_sends_campaign_sent_idx ON email_sends (campaign_id, sent_at);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_sends_variant_idx ON email_sends (variant_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_sends_account_idx ON email_sends (sending_account_email, sent_at);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX email_sends_instantly_email_idx ON email_sends (instantly_email_id) WHERE instantly_email_id IS NOT NULL;
-- +goose StatementEnd

-- Every provider delivery, raw, keyed by a content hash so a redelivery is a no-op.
-- Neither Instantly nor Mailchimp sends an event id.
-- +goose StatementBegin
CREATE TABLE provider_events (
    id               uuid PRIMARY KEY,
    provider         text NOT NULL CHECK (provider IN ('instantly', 'mailchimp')),
    event_type       text NOT NULL,
    dedupe_key       text NOT NULL UNIQUE,
    received_at      timestamptz NOT NULL DEFAULT now(),
    occurred_at      timestamptz,
    raw              jsonb NOT NULL,
    source           text NOT NULL CHECK (source IN ('webhook', 'replay', 'test')),
    campaign_id      uuid,
    contact_id       uuid,
    campaign_lead_id uuid,
    processed_at     timestamptz,
    attempts         integer NOT NULL DEFAULT 0,
    error            text
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX provider_events_unprocessed_idx ON provider_events (received_at) WHERE processed_at IS NULL;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX provider_events_contact_idx ON provider_events (contact_id, received_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX provider_events_received_idx ON provider_events (received_at DESC);
-- +goose StatementEnd

-- The unified timeline. One row per thing that happened to a contact, whichever
-- provider or person caused it.
-- +goose StatementBegin
CREATE TABLE contact_events (
    id                bigserial PRIMARY KEY,
    contact_id        uuid NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    campaign_id       uuid REFERENCES campaigns (id) ON DELETE SET NULL,
    campaign_lead_id  uuid REFERENCES campaign_leads (id) ON DELETE CASCADE,
    assignment_id     uuid,
    variant_id        uuid,
    send_id           uuid,
    step              integer,
    type              text NOT NULL CHECK (type IN (
                          'imported', 'queued', 'pushed', 'push_failed', 'sent', 'opened', 'clicked',
                          'replied', 'auto_replied', 'interested', 'not_interested', 'wrong_person',
                          'meeting_booked', 'bounced', 'unsubscribed', 'skipped', 'suppressed',
                          'suppression_lifted', 'permission_requested', 'consent_captured',
                          'consent_revoked', 'newsletter_eligible', 'newsletter_pushed',
                          'newsletter_pending', 'newsletter_subscribed', 'newsletter_unsubscribed',
                          'newsletter_cleaned', 'newsletter_profile_updated', 'stage_changed', 'note')),
    occurred_at       timestamptz NOT NULL,
    source            text NOT NULL CHECK (source IN (
                          'instantly_webhook', 'mailchimp_webhook', 'reconcile', 'manual', 'system', 'import')),
    provider_event_id uuid REFERENCES provider_events (id) ON DELETE SET NULL,
    stage_before      text,
    stage_after       text,
    data              jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contact_events_contact_idx ON contact_events (contact_id, occurred_at, id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contact_events_campaign_idx ON contact_events (campaign_id, occurred_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX contact_events_type_idx ON contact_events (type, occurred_at);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX contact_events_provider_idx ON contact_events (provider_event_id, type)
    WHERE provider_event_id IS NOT NULL;
-- +goose StatementEnd

-- A Mailchimp audience, mirrored, plus our own consent policy for it.
-- allow_single_opt_in is ours and defaults to off: a contact is pushed as pending
-- (double opt-in) unless an operator turns this on for the audience.
-- +goose StatementBegin
CREATE TABLE newsletter_audiences (
    id                  uuid PRIMARY KEY,
    mailchimp_list_id   text NOT NULL UNIQUE,
    name                text NOT NULL,
    double_optin        boolean NOT NULL DEFAULT true,
    allow_single_opt_in boolean NOT NULL DEFAULT false,
    default_tags        text[] NOT NULL DEFAULT '{}',
    webhook_token       text UNIQUE,
    webhook_id          text,
    webhook_secret_enc  bytea,
    webhook_url         text,
    member_count        integer,
    stats               jsonb NOT NULL DEFAULT '{}'::jsonb,
    is_default          boolean NOT NULL DEFAULT false,
    last_synced_at      timestamptz,
    last_sync_error     text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX newsletter_audiences_default_idx ON newsletter_audiences (is_default) WHERE is_default;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaign_settings ADD CONSTRAINT campaign_settings_default_audience_fk
    FOREIGN KEY (default_audience_id) REFERENCES newsletter_audiences (id) ON DELETE SET NULL;
-- +goose StatementEnd

-- One contact in one audience. requested_status is what we asked Mailchimp for;
-- status is what Mailchimp says. A subscribed request needs a consent record.
-- +goose StatementBegin
CREATE TABLE newsletter_subscriptions (
    id                   uuid PRIMARY KEY,
    contact_id           uuid NOT NULL REFERENCES contacts (id) ON DELETE RESTRICT,
    audience_id          uuid NOT NULL REFERENCES newsletter_audiences (id) ON DELETE RESTRICT,
    consent_id           uuid REFERENCES contact_consents (id) ON DELETE RESTRICT,
    requested_status     text NOT NULL CHECK (requested_status IN ('pending', 'subscribed', 'unsubscribed')),
    status               text NOT NULL DEFAULT 'local_pending'
                         CHECK (status IN ('local_pending', 'pending', 'subscribed', 'unsubscribed', 'cleaned',
                                           'transactional', 'archived', 'compliance_blocked', 'error')),
    sync_status          text NOT NULL DEFAULT 'queued'
                         CHECK (sync_status IN ('queued', 'syncing', 'synced', 'failed')),
    subscriber_hash      text,
    unique_email_id      text,
    mailchimp_contact_id text,
    web_id               bigint,
    claimed_at           timestamptz,
    pushed_at            timestamptz,
    last_synced_at       timestamptz,
    last_error           text,
    sync_attempts        integer NOT NULL DEFAULT 0,
    subscribed_at        timestamptz,
    unsubscribed_at      timestamptz,
    unsubscribe_reason   text,
    tags                 text[] NOT NULL DEFAULT '{}',
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contact_id, audience_id),
    UNIQUE (audience_id, subscriber_hash),
    CHECK (requested_status <> 'subscribed' OR consent_id IS NOT NULL)
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX newsletter_subscriptions_sync_idx ON newsletter_subscriptions (sync_status, updated_at)
    WHERE sync_status IN ('queued', 'failed');
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX newsletter_subscriptions_status_idx ON newsletter_subscriptions (status);
-- +goose StatementEnd

-- Daily analytics as the provider reports them, kept beside our own event-derived
-- numbers so the two can be compared, never merged.
-- +goose StatementBegin
CREATE TABLE campaign_analytics_snapshots (
    id          uuid PRIMARY KEY,
    campaign_id uuid NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    day         date NOT NULL,
    source      text NOT NULL CHECK (source IN ('instantly', 'local')),
    metrics     jsonb NOT NULL,
    fetched_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (campaign_id, day, source)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE sending_account_stats_daily (
    sending_account_id  uuid NOT NULL REFERENCES sending_accounts (id) ON DELETE CASCADE,
    day                 date NOT NULL,
    sent                integer NOT NULL DEFAULT 0,
    bounced             integer NOT NULL DEFAULT 0,
    contacted           integer NOT NULL DEFAULT 0,
    new_leads_contacted integer NOT NULL DEFAULT 0,
    opened              integer NOT NULL DEFAULT 0,
    unique_opened       integer NOT NULL DEFAULT 0,
    replies             integer NOT NULL DEFAULT 0,
    unique_replies      integer NOT NULL DEFAULT 0,
    clicks              integer NOT NULL DEFAULT 0,
    unique_clicks       integer NOT NULL DEFAULT 0,
    fetched_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (sending_account_id, day)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE sync_runs (
    id            uuid PRIMARY KEY,
    kind          text NOT NULL CHECK (kind IN (
                      'instantly_campaign', 'instantly_campaigns_all', 'instantly_leads_full',
                      'instantly_accounts', 'instantly_webhook_replay', 'mailchimp_members',
                      'mailchimp_audiences')),
    status        text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'done', 'failed')),
    target_id     uuid,
    started_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz,
    items_seen    integer NOT NULL DEFAULT 0,
    items_updated integer NOT NULL DEFAULT 0,
    error         text,
    details       jsonb NOT NULL DEFAULT '{}'::jsonb
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX sync_runs_kind_idx ON sync_runs (kind, started_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS sync_runs;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS sending_account_stats_daily;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS campaign_analytics_snapshots;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS newsletter_subscriptions;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE campaign_settings DROP CONSTRAINT IF EXISTS campaign_settings_default_audience_fk;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS newsletter_audiences;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS contact_events;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS provider_events;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS email_sends;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER IF EXISTS variant_assignments_guard ON variant_assignments;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS variant_assignments_guard();
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS variant_assignments;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS campaign_variants;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS email_variant_components;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS email_variants;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS email_components;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS ai_generations;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_campaign_lead_fk;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE contact_suppressions DROP CONSTRAINT IF EXISTS contact_suppressions_campaign_lead_fk;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS campaign_leads;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS campaign_sending_accounts;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS campaigns;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS sending_accounts;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER IF EXISTS contacts_guard_stage ON contacts;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS contacts_guard_stage();
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS contact_suppressions;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS contact_consents;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS contacts;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS campaign_settings;
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM sources WHERE kind IN ('instantly', 'mailchimp');
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_kind_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_kind_check CHECK (kind IN ('apify', 'outscraper', 'emailable'));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_role_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sources ADD CONSTRAINT sources_role_check CHECK (role IN ('maps', 'verifier'));
-- +goose StatementEnd
