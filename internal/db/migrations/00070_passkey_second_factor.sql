-- +goose Up
-- A passkey as second factor after a password sign-in: the user opts in,
-- and a password login then issues a short-lived challenge that a passkey
-- assertion satisfies before a session is created.
ALTER TABLE users ADD COLUMN passkey_second_factor boolean NOT NULL DEFAULT false;

CREATE TABLE login_challenges (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash    bytea NOT NULL UNIQUE,            -- SHA-256 of the challenge token
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    satisfied_at  timestamptz,                      -- passkey assertion verified
    consumed_at   timestamptz                       -- session issued
);
CREATE INDEX login_challenges_expires_idx ON login_challenges (expires_at);

-- +goose Down
DROP TABLE IF EXISTS login_challenges;
ALTER TABLE users DROP COLUMN passkey_second_factor;
