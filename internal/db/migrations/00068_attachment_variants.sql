-- +goose Up
-- Display variants of a photo (thumb/medium JPEG keys) and a blurhash
-- placeholder, derived server-side; empty until generated (backfill command).
ALTER TABLE activity_attachments
    ADD COLUMN thumb_blob_id  text NOT NULL DEFAULT '',
    ADD COLUMN medium_blob_id text NOT NULL DEFAULT '',
    ADD COLUMN blurhash       text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE activity_attachments
    DROP COLUMN thumb_blob_id,
    DROP COLUMN medium_blob_id,
    DROP COLUMN blurhash;
