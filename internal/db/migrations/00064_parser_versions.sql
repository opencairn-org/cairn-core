-- +goose Up
-- Provider versions (docs/adr/0005): source_worker_version is the parser
-- version "N.M" (legacy "N" = N.0); the worker release that wrote the
-- source is stamped for diagnostics.
ALTER TABLE activity_sources ADD COLUMN source_worker_release text NOT NULL DEFAULT '';

-- +goose StatementBegin
-- {dataset, parser} for ordering; NULL when v is not N or N.M.
CREATE FUNCTION cairn_parser_version(v text) RETURNS int[]
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT CASE WHEN v ~ '^[0-9]+(\.[0-9]+)?$'
        THEN ARRAY[split_part(v, '.', 1)::int, COALESCE(NULLIF(split_part(v, '.', 2), ''), '0')::int]
    END
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION cairn_parser_version(text);
ALTER TABLE activity_sources DROP COLUMN source_worker_release;
