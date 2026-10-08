-- +goose Up
-- Sign in with ChatGPT: the operator connects one ChatGPT account and AI
-- generations run against that account's ChatGPT plan instead of an API key.
--
-- One connection for the whole install, like workspace_settings: the row exists
-- only while an account is connected and disconnecting deletes it.
--
--   connected        tokens are usable; the access token is refreshed on demand
--   needs_reconnect  OpenAI rejected the refresh token or the access token (the
--                    user revoked access, or 30 days passed unused); the operator
--                    has to sign in again
--
-- Both tokens are AES-GCM encrypted with KARVON_SECRET_KEY. The refresh token is
-- rotated on every refresh, so a refresh and its write happen under a row lock.
-- +goose StatementBegin
CREATE TABLE ai_chatgpt_connection (
    id                smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    status            text NOT NULL DEFAULT 'connected' CHECK (status IN ('connected', 'needs_reconnect')),
    subject           text NOT NULL,
    email             text,
    scope             text NOT NULL DEFAULT '',
    access_token_enc  bytea NOT NULL,
    refresh_token_enc bytea NOT NULL,
    access_expires_at timestamptz NOT NULL,
    last_error        text,
    connected_at      timestamptz NOT NULL DEFAULT now(),
    refreshed_at      timestamptz,
    updated_at        timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- One row per sign-in attempt between "Connect" and the callback. The state is
-- stored hashed: it is the only thing the public callback route trusts.
-- +goose StatementBegin
CREATE TABLE ai_chatgpt_oauth_states (
    state_hash        text PRIMARY KEY,
    code_verifier_enc bytea NOT NULL,
    nonce             text NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE ai_generations DROP CONSTRAINT ai_generations_provider_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE ai_generations ADD CONSTRAINT ai_generations_provider_check
    CHECK (provider IN ('manual_chatgpt', 'openai_api', 'chatgpt_plan'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE ai_generations SET provider = 'openai_api' WHERE provider = 'chatgpt_plan';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE ai_generations DROP CONSTRAINT ai_generations_provider_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE ai_generations ADD CONSTRAINT ai_generations_provider_check
    CHECK (provider IN ('manual_chatgpt', 'openai_api'));
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS ai_chatgpt_oauth_states;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS ai_chatgpt_connection;
-- +goose StatementEnd
