-- +goose Up
-- RFC 8707 resource indicators: codes and tokens are bound to one resource.
-- '' is the REST/Connect API (and every token issued before this), 'mcp' the
-- MCP endpoint; each only accepts tokens of its own audience.
ALTER TABLE oauth_authorization_codes ADD COLUMN audience text NOT NULL DEFAULT '';
ALTER TABLE oauth_access_tokens ADD COLUMN audience text NOT NULL DEFAULT '';
ALTER TABLE oauth_refresh_tokens ADD COLUMN audience text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE oauth_refresh_tokens DROP COLUMN audience;
ALTER TABLE oauth_access_tokens DROP COLUMN audience;
ALTER TABLE oauth_authorization_codes DROP COLUMN audience;
