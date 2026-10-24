-- +goose Up
-- Acceptance operation IDs replay their answer while retained (US-005,
-- CTRL-IDEM-01). owner_account_id is the accepting account and the answer is
-- status 'accepted' with the tenant's canvas, so a replay names exactly the
-- canvas the first acceptance granted. Each kind keeps only its own answer.
ALTER TABLE invitation_operations
    ADD COLUMN canvas_id text CHECK (canvas_id <> '' AND octet_length(canvas_id) <= 128),
    DROP CONSTRAINT invitation_operations_kind_check,
    ADD CONSTRAINT invitation_operations_kind_check CHECK (kind IN ('create', 'revoke', 'accept')),
    DROP CONSTRAINT invitation_operations_answer_check,
    ADD CONSTRAINT invitation_operations_answer_check CHECK (CASE kind
        WHEN 'create' THEN created_at IS NOT NULL AND expires_at IS NOT NULL AND status IS NULL AND canvas_id IS NULL
        WHEN 'revoke' THEN created_at IS NULL AND expires_at IS NULL AND status IS NOT NULL AND canvas_id IS NULL
        WHEN 'accept' THEN created_at IS NULL AND expires_at IS NULL AND status IS NOT DISTINCT FROM 'accepted' AND canvas_id IS NOT NULL
        ELSE false
    END);

-- +goose Down
-- 00009 cannot hold accept records. They only replay an answer for ten
-- minutes, so they are dropped; the memberships and consumed invitations they
-- describe stay, and a late retry is refused as an unusable link.
DELETE FROM invitation_operations WHERE kind = 'accept';
ALTER TABLE invitation_operations
    DROP CONSTRAINT invitation_operations_answer_check,
    DROP CONSTRAINT invitation_operations_kind_check,
    DROP COLUMN canvas_id,
    ADD CONSTRAINT invitation_operations_kind_check CHECK (kind IN ('create', 'revoke')),
    ADD CONSTRAINT invitation_operations_answer_check CHECK (CASE kind
        WHEN 'create' THEN created_at IS NOT NULL AND expires_at IS NOT NULL AND status IS NULL
        ELSE created_at IS NULL AND expires_at IS NULL AND status IS NOT NULL
    END);
