-- +goose Up
-- Instantly's per-day figures for a campaign (GET /campaigns/analytics/daily), kept
-- for the campaigns started in Instantly's own app. Those send nothing through
-- Karvon, so they have no email_sends or contact_events to count, and their
-- analytics snapshots are lifetime totals as of each fetch: neither can say what
-- happened inside a date window. These rows can, which is what the dashboard needs.
--
-- A day is the calendar day Instantly reports the activity on: sends on the day
-- they went out, replies and opportunities on the day they came in. Each sync
-- re-fetches a trailing window and overwrites it, so late figures settle.
-- +goose StatementBegin
CREATE TABLE campaign_stats_daily (
    campaign_id         uuid NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    day                 date NOT NULL,
    sent                integer NOT NULL DEFAULT 0,
    contacted           integer NOT NULL DEFAULT 0,
    new_leads_contacted integer NOT NULL DEFAULT 0,
    opened              integer NOT NULL DEFAULT 0,
    unique_opened       integer NOT NULL DEFAULT 0,
    replies             integer NOT NULL DEFAULT 0,
    unique_replies      integer NOT NULL DEFAULT 0,
    clicks              integer NOT NULL DEFAULT 0,
    unique_clicks       integer NOT NULL DEFAULT 0,
    opportunities       integer NOT NULL DEFAULT 0,
    fetched_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (campaign_id, day)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX campaign_stats_daily_day_idx ON campaign_stats_daily (day);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS campaign_stats_daily;
-- +goose StatementEnd
