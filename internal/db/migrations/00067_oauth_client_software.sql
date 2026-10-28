-- +goose Up
-- RFC 7591 software_id / software_version of dynamically registered clients, so
-- an "authorized apps" view can name the application behind a client row.
ALTER TABLE oauth_clients
    ADD COLUMN software_id      text NOT NULL DEFAULT '',
    ADD COLUMN software_version text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE oauth_clients
    DROP COLUMN software_id,
    DROP COLUMN software_version;
