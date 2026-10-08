-- name: GetChatGPTConnection :one
SELECT * FROM ai_chatgpt_connection WHERE id = 1;

-- name: LockChatGPTConnection :one
SELECT * FROM ai_chatgpt_connection WHERE id = 1 FOR UPDATE;

-- name: UpsertChatGPTConnection :one
INSERT INTO ai_chatgpt_connection (id, status, subject, email, scope, access_token_enc, refresh_token_enc,
                                   access_expires_at, last_error, connected_at, refreshed_at, updated_at)
VALUES (1, 'connected', sqlc.arg('subject'), sqlc.narg('email'), sqlc.arg('scope'), sqlc.arg('access_token_enc'),
        sqlc.arg('refresh_token_enc'), sqlc.arg('access_expires_at'), NULL, now(), NULL, now())
ON CONFLICT (id) DO UPDATE
SET status            = 'connected',
    subject           = EXCLUDED.subject,
    email             = EXCLUDED.email,
    scope             = EXCLUDED.scope,
    access_token_enc  = EXCLUDED.access_token_enc,
    refresh_token_enc = EXCLUDED.refresh_token_enc,
    access_expires_at = EXCLUDED.access_expires_at,
    last_error        = NULL,
    connected_at      = now(),
    refreshed_at      = NULL,
    updated_at        = now()
RETURNING *;

-- name: SetChatGPTTokens :exec
UPDATE ai_chatgpt_connection
SET access_token_enc  = sqlc.arg('access_token_enc'),
    refresh_token_enc = sqlc.arg('refresh_token_enc'),
    access_expires_at = sqlc.arg('access_expires_at'),
    scope             = CASE WHEN sqlc.arg('scope')::text = '' THEN scope ELSE sqlc.arg('scope')::text END,
    last_error        = NULL,
    refreshed_at      = now(),
    updated_at        = now()
WHERE id = 1;

-- name: SetChatGPTNeedsReconnect :exec
UPDATE ai_chatgpt_connection
SET status     = 'needs_reconnect',
    last_error = sqlc.arg('last_error'),
    updated_at = now()
WHERE id = 1;

-- name: DeleteChatGPTConnection :exec
DELETE FROM ai_chatgpt_connection WHERE id = 1;

-- name: CreateChatGPTOAuthState :exec
INSERT INTO ai_chatgpt_oauth_states (state_hash, code_verifier_enc, nonce, expires_at)
VALUES (sqlc.arg('state_hash'), sqlc.arg('code_verifier_enc'), sqlc.arg('nonce'), sqlc.arg('expires_at'));

-- name: TakeChatGPTOAuthState :one
DELETE FROM ai_chatgpt_oauth_states
WHERE state_hash = sqlc.arg('state_hash')
RETURNING *;

-- name: PruneChatGPTOAuthStates :exec
DELETE FROM ai_chatgpt_oauth_states WHERE expires_at <= sqlc.arg('now');

-- name: ExpireChatGPTAccessToken :exec
UPDATE ai_chatgpt_connection
SET access_expires_at = sqlc.arg('at'),
    updated_at        = now()
WHERE id = 1;
