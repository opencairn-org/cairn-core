-- +goose Up
-- A login challenge now also carries a forced password change: a password
-- sign-in of a user flagged must_change_password yields a challenge that a
-- new password completes before any session is issued.
ALTER TABLE login_challenges
    ADD COLUMN purpose text NOT NULL DEFAULT 'second_factor'
        CHECK (purpose IN ('second_factor', 'password_change'));

-- +goose Down
ALTER TABLE login_challenges DROP COLUMN purpose;
