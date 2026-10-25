-- +goose Up
-- One provider account may be connected once per worker pool (docs/adr/0006):
-- specialised importers each get their own connection, and a user who does
-- not want a pool simply does not connect it.
DROP INDEX external_accounts_provider_account_uniq;
CREATE UNIQUE INDEX external_accounts_provider_account_pool_uniq
    ON external_accounts (user_id, provider, provider_account_id, (COALESCE(assigned_worker_name, '')))
    WHERE provider_account_id IS NOT NULL;

-- +goose Down
DROP INDEX external_accounts_provider_account_pool_uniq;
CREATE UNIQUE INDEX external_accounts_provider_account_uniq
    ON external_accounts (user_id, provider, provider_account_id)
    WHERE provider_account_id IS NOT NULL;
