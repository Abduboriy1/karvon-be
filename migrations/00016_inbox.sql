-- +goose Up
-- The inbox: a local mirror of the emails Instantly's Unibox shows, so replies can
-- be read, searched and joined to campaign outcomes without a round trip.
--
-- Every received email is mirrored by the inbox sync (a periodic pass, plus one
-- queued by each reply webhook). Sent emails are only mirrored when a thread is
-- opened, so a conversation reads in full without copying every campaign send.
-- The row id is Instantly's own email id, which makes every sync an upsert.
-- +goose StatementBegin
CREATE TABLE inbox_emails (
    id                    text PRIMARY KEY,
    thread_id             text,
    message_id            text,
    direction             text NOT NULL CHECK (direction IN ('received', 'sent')),
    ue_type               integer,
    email_account         citext,
    lead_email            citext,
    from_address          citext,
    to_addresses          text,
    cc_addresses          text,
    subject               text,
    body_text             text,
    body_html             text,
    content_preview       text,
    step                  text,
    is_unread             boolean NOT NULL DEFAULT false,
    is_auto_reply         boolean NOT NULL DEFAULT false,
    interest_status       integer,
    ai_interest_value     double precision,
    instantly_campaign_id text,
    campaign_id           uuid REFERENCES campaigns (id) ON DELETE SET NULL,
    contact_id            uuid REFERENCES contacts (id) ON DELETE SET NULL,
    sent_at               timestamptz NOT NULL,
    provider_created_at   timestamptz NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX inbox_emails_sent_idx ON inbox_emails (sent_at DESC) WHERE direction = 'received';
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX inbox_emails_thread_idx ON inbox_emails (thread_id, sent_at);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX inbox_emails_lead_idx ON inbox_emails (lead_email, sent_at DESC) WHERE direction = 'received';
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX inbox_emails_created_idx ON inbox_emails (provider_created_at DESC) WHERE direction = 'received';
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX inbox_emails_unread_idx ON inbox_emails (sent_at DESC) WHERE direction = 'received' AND is_unread;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX inbox_emails_contact_idx ON inbox_emails (contact_id) WHERE contact_id IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sync_runs DROP CONSTRAINT IF EXISTS sync_runs_kind_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sync_runs ADD CONSTRAINT sync_runs_kind_check CHECK (kind IN (
    'instantly_campaign', 'instantly_campaigns_all', 'instantly_leads_full',
    'instantly_accounts', 'instantly_webhook_replay', 'instantly_inbox', 'mailchimp_members',
    'mailchimp_audiences'));
-- +goose StatementEnd

-- Negative Instantly interest codes (not interested is -1, wrong person -2, lost
-- -3, no-show -4) were narrowed to 0, which reads as out-of-office. The label was
-- always stored correctly, so the code is restored from it.
-- +goose StatementBegin
UPDATE campaign_leads
SET interest_status = CASE interest_label
        WHEN 'not_interested' THEN -1
        WHEN 'wrong_person' THEN -2
        WHEN 'lost' THEN -3
        WHEN 'no_show' THEN -4
    END,
    updated_at = now()
WHERE interest_status = 0 AND interest_label IN ('not_interested', 'wrong_person', 'lost', 'no_show');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM sync_runs WHERE kind = 'instantly_inbox';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sync_runs DROP CONSTRAINT IF EXISTS sync_runs_kind_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sync_runs ADD CONSTRAINT sync_runs_kind_check CHECK (kind IN (
    'instantly_campaign', 'instantly_campaigns_all', 'instantly_leads_full',
    'instantly_accounts', 'instantly_webhook_replay', 'mailchimp_members',
    'mailchimp_audiences'));
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS inbox_emails;
-- +goose StatementEnd
