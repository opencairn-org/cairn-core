-- +goose Up
-- Admin-triggered maintenance jobs (TriggerMaintenance). The runner keeps the
-- live state in memory and writes through here so the history survives a
-- restart; queued jobs are picked up again, a job interrupted while running
-- is marked failed (the work is idempotent, re-trigger it).
CREATE TABLE maintenance_jobs (
    id           uuid PRIMARY KEY,
    operation    text NOT NULL,
    user_ids     uuid[] NOT NULL DEFAULT '{}'::uuid[],
    status       text NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    estimated    bigint NOT NULL DEFAULT 0,
    processed    bigint NOT NULL DEFAULT 0,
    failed       bigint NOT NULL DEFAULT 0,
    error        text NOT NULL DEFAULT '',
    triggered_by uuid REFERENCES users(id) ON DELETE SET NULL,
    queued_at    timestamptz NOT NULL,
    started_at   timestamptz,
    finished_at  timestamptz,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX maintenance_jobs_queued_idx ON maintenance_jobs (queued_at DESC);

-- +goose Down
DROP TABLE maintenance_jobs;
