-- match_constraints also holds matcher suggestions: a high-scoring same-pool
-- pair the one-per-pool rule kept apart, surfaced as a "possible duplicate"
-- review item until the owner merges (must_link) or keeps both (cannot_link).

-- +goose Up
ALTER TABLE match_constraints
    DROP CONSTRAINT match_constraints_kind_check,
    ADD CONSTRAINT match_constraints_kind_check
        CHECK (kind IN ('must_link','cannot_link','suspected_duplicate'));

-- +goose Down
DELETE FROM match_constraints WHERE kind = 'suspected_duplicate';
ALTER TABLE match_constraints
    DROP CONSTRAINT match_constraints_kind_check,
    ADD CONSTRAINT match_constraints_kind_check
        CHECK (kind IN ('must_link','cannot_link'));
