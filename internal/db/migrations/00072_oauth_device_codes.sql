-- +goose Up
-- RFC 8628 device authorization: a device that cannot open a browser shows a
-- short user code that a signed-in client approves; the device polls the token
-- endpoint with the long device code. Only the device code's hash is stored.
CREATE TABLE oauth_device_codes (
    device_code_hash bytea PRIMARY KEY,
    user_code        text NOT NULL UNIQUE,          -- live codes are unique; expired rows are purged on insert
    client_id        text NOT NULL,
    scope            text NOT NULL DEFAULT '',      -- requested, filtered to the client's scopes
    status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'denied')),
    user_id          uuid REFERENCES users(id) ON DELETE CASCADE,
    granted_scope    text NOT NULL DEFAULT '',
    expires_at       timestamptz NOT NULL,
    decided_at       timestamptz,
    consumed_at      timestamptz,
    last_polled_at   timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE oauth_device_codes;
