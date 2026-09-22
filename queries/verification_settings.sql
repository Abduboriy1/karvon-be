-- name: GetVerificationSettings :one
SELECT * FROM verification_settings WHERE id = 1;

-- name: UpsertVerificationSettings :one
-- One row, always id 1. The service validates the weights before calling this; the
-- column constraints only catch the thresholds, because "the weights total 100" is
-- not expressible against a JSONB object without a function.
INSERT INTO verification_settings (id, weights, enabled, paid_enabled, paid_threshold,
                                   paid_min_score, updated_at)
VALUES (1, sqlc.arg('weights'), sqlc.arg('enabled'), sqlc.arg('paid_enabled'),
        sqlc.arg('paid_threshold'), sqlc.arg('paid_min_score'), now())
ON CONFLICT (id) DO UPDATE
    SET weights        = excluded.weights,
        enabled        = excluded.enabled,
        paid_enabled   = excluded.paid_enabled,
        paid_threshold = excluded.paid_threshold,
        paid_min_score = excluded.paid_min_score,
        updated_at     = now()
RETURNING *;
