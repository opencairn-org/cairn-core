-- +goose Up
-- Source reimport previews become a two-phase record: a pending row claims
-- the next worker result for its source, then holds the proposed payload and
-- the diffs until committed, discarded or expired.
ALTER TABLE activity_source_previews
    ADD COLUMN job_id text NOT NULL DEFAULT '',
    ADD COLUMN status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ready')),
    ALTER COLUMN parsed SET DEFAULT '{}'::jsonb,
    ALTER COLUMN source_diff SET DEFAULT '[]'::jsonb,
    ALTER COLUMN merged_diff SET DEFAULT '[]'::jsonb;

CREATE INDEX activity_source_previews_pending_idx
    ON activity_source_previews (activity_source_id) WHERE status = 'pending';
CREATE INDEX activity_source_previews_expires_idx ON activity_source_previews (expires_at);

-- +goose Down
DROP INDEX IF EXISTS activity_source_previews_expires_idx;
DROP INDEX IF EXISTS activity_source_previews_pending_idx;
ALTER TABLE activity_source_previews
    DROP COLUMN job_id,
    DROP COLUMN status,
    ALTER COLUMN parsed DROP DEFAULT,
    ALTER COLUMN source_diff DROP DEFAULT,
    ALTER COLUMN merged_diff DROP DEFAULT;
