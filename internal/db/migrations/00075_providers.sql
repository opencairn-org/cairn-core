-- +goose Up
-- Provider meta (display, auth method, OAuth authorize endpoint, connect-form
-- fields) as declared by the provider's worker manifest. Replaces the
-- compiled-in registry; bound to the first package that published it.
CREATE TABLE providers (
    provider   text PRIMARY KEY,
    package    text,            -- bound package; NULL until a worker publishes
    meta       jsonb NOT NULL,  -- domain.ProviderMeta
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- The two providers core used to compile in, so connections render and the
-- connect form works until their 0.7 workers publish a manifest.
INSERT INTO providers (provider, meta) VALUES
('strava', '{"display_name":"Strava","activity_url_template":"https://www.strava.com/activities/{id}","auth_method":"oauth2","oauth":{"authorize_url":"https://www.strava.com/oauth/authorize","scopes":["read","activity:read_all"],"scope_separator":",","authorize_params":{"approval_prompt":"force"}},"config_fields":[{"key":"client_id","label":"Client ID","help":"From your Strava API application."},{"key":"client_secret","label":"Client secret","secret":true}]}'),
('garmin', '{"display_name":"Garmin","activity_url_template":"https://connect.garmin.com/modern/activity/{id}","auth_method":"credentials","config_fields":[{"key":"client_id","label":"Garmin Connect email"},{"key":"client_secret","label":"Password","secret":true,"help":"Garmin has no public API, so Cairn signs in with your Garmin Connect email and password (stored encrypted). MFA accounts may need a one-time device approval. The account starts importing right after you create the connection."}]}');

-- +goose Down
DROP TABLE providers;
