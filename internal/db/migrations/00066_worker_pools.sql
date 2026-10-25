-- +goose Up
-- A worker pool (provider + name) is a permanent identity bound to one
-- package (docs/adr/0006): the set of known pools core routes accounts to,
-- and the check that a name is never reused for another implementation.
CREATE TABLE worker_pools (
    provider      text NOT NULL,
    name          text NOT NULL,
    package       text NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, name)
);
-- Pools already in use: every (provider, name, package) that produced a source.
INSERT INTO worker_pools (provider, name, package, first_seen_at, last_seen_at)
SELECT provider, source_worker_name, source_worker_package, min(imported_at), max(updated_at)
  FROM activity_sources
 WHERE source_worker_name <> '' AND source_worker_package <> ''
 GROUP BY provider, source_worker_name, source_worker_package
 ON CONFLICT DO NOTHING;

-- +goose Down
DROP TABLE worker_pools;
