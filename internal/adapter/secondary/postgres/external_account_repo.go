package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ExternalAccountRepo implements port.ExternalAccountRepo on top of the
// external_accounts table from migration 4.
//
// Token columns (access_token_encrypted, refresh_token_encrypted) are
// deliberately NOT loaded through scanExternalAccountRow — the token-
// fetch path lives in a separate repository (TokenRepo, dedicated to
// the OAuth-refresh use case) so the rest of the codebase has no
// chance of accidentally serialising a token into a domain.Activity
// or a notification payload.
type ExternalAccountRepo struct {
	pool *pgxpool.Pool

	// Schedule knobs read from config (see SchedulerConfig). Held on
	// the repo struct so the SQL in ListAccountsDueForReconcile can
	// reference them as parameters rather than reading instance_settings
	// on every call.
	pollingInterval      time.Duration
	webhookDriftInterval time.Duration
}

// NewExternalAccountRepo wires the repo. pollingInterval and
// webhookDriftInterval come from SchedulerConfig; pass zeros to fall
// back to sensible defaults (60 min / 24 h).
func NewExternalAccountRepo(
	pool *pgxpool.Pool,
	pollingInterval, webhookDriftInterval time.Duration,
) *ExternalAccountRepo {
	if pollingInterval <= 0 {
		pollingInterval = 60 * time.Minute
	}
	if webhookDriftInterval <= 0 {
		webhookDriftInterval = 24 * time.Hour
	}
	return &ExternalAccountRepo{
		pool:                 pool,
		pollingInterval:      pollingInterval,
		webhookDriftInterval: webhookDriftInterval,
	}
}

// CreateAccount inserts a new external_accounts row, or on re-connect
// (same user+provider+provider_account_id+worker pool) reuses the existing
// one. A pre-0.5 row without a pool is adopted by the first pool that
// connects it. Returns the row id either way.
func (r *ExternalAccountRepo) CreateAccount(ctx context.Context, a domain.ExternalAccount) (domain.ExternalAccountID, error) {
	label := a.DisplayLabel
	if label == "" {
		label = a.Provider
	}
	scopes := a.GrantedScopes
	if scopes == nil {
		scopes = []string{}
	}
	var providerAcct any
	if a.ProviderAccountID != "" {
		providerAcct = a.ProviderAccountID
	}
	var connectionID any
	if a.ConnectionID != nil {
		connectionID = a.ConnectionID.UUID()
	}
	db := dbtx(ctx, r.pool)
	if a.AssignedWorkerName != "" && a.ProviderAccountID != "" {
		if _, err := db.Exec(ctx, `
			UPDATE external_accounts e SET assigned_worker_name = $4, updated_at = now()
			 WHERE e.user_id = $1 AND e.provider = $2 AND e.provider_account_id = $3
			   AND e.assigned_worker_name IS NULL
			   AND NOT EXISTS (SELECT 1 FROM external_accounts o
			                    WHERE o.user_id = $1 AND o.provider = $2 AND o.provider_account_id = $3
			                      AND o.assigned_worker_name = $4)
			   -- its history must belong to this pool: same connection, or
			   -- every source was imported under this pool's name
			   AND (e.connection_id IS NOT DISTINCT FROM $5
			        OR NOT EXISTS (SELECT 1 FROM activity_sources s
			                        WHERE s.external_account_id = e.id AND s.source_worker_name <> $4))`,
			a.UserID.UUID(), a.Provider, a.ProviderAccountID, a.AssignedWorkerName, connectionID); err != nil {
			return domain.ExternalAccountID{}, fmt.Errorf("adopt external account: %w", err)
		}
	}
	const q = `
		INSERT INTO external_accounts (user_id, provider, provider_account_id, connection_id, display_label, granted_scopes, status, assigned_worker_name)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', NULLIF($7, ''))
		ON CONFLICT (user_id, provider, provider_account_id, (COALESCE(assigned_worker_name, ''))) WHERE provider_account_id IS NOT NULL
		DO UPDATE SET
			connection_id = EXCLUDED.connection_id,
			display_label = EXCLUDED.display_label,
			granted_scopes = EXCLUDED.granted_scopes,
			status = 'active',
			status_reason = NULL,
			updated_at = now()
		RETURNING id`
	var id uuid.UUID
	if err := db.QueryRow(ctx, q, a.UserID.UUID(), a.Provider, providerAcct, connectionID, label, scopes, a.AssignedWorkerName).Scan(&id); err != nil {
		return domain.ExternalAccountID{}, fmt.Errorf("create external account: %w", err)
	}
	return domain.ExternalAccountID(id), nil
}

// externalAccountColumns lists every column except the encrypted token
// pair. Order is fixed; scanExternalAccountRow relies on it.
const externalAccountColumns = `
	id, user_id, provider, provider_account_id, connection_id,
	display_label, assigned_worker_name,
	status, status_reason,
	config, granted_scopes,
	webhook_subscribed, webhook_subscribed_at, webhook_subscription_id,
	rate_limit,
	sync_watermark, last_sync_at, last_successful_sync_at,
	created_at, updated_at,
	auto_import_enabled
`

// GetExternalAccount returns one account by ID. Returns domain.ErrNotFound
// when missing.
func (r *ExternalAccountRepo) GetExternalAccount(
	ctx context.Context,
	id domain.ExternalAccountID,
) (domain.ExternalAccount, error) {
	db := dbtx(ctx, r.pool)
	row := db.QueryRow(ctx,
		`SELECT `+externalAccountColumns+`
		   FROM external_accounts
		  WHERE id = $1`,
		id.UUID(),
	)
	a, err := scanExternalAccountRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ExternalAccount{}, fmt.Errorf("get external account %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return domain.ExternalAccount{}, fmt.Errorf("get external account %s: %w", id, err)
	}
	return a, nil
}

// ListAccountsForUser returns every external account belonging to the user,
// ordered by provider then display_label.
func (r *ExternalAccountRepo) ListAccountsForUser(
	ctx context.Context,
	userID domain.UserID,
) ([]domain.ExternalAccount, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT `+externalAccountColumns+`
		   FROM external_accounts
		  WHERE user_id = $1
		  ORDER BY provider ASC, display_label ASC`,
		userID.UUID(),
	)
	if err != nil {
		return nil, fmt.Errorf("list accounts for user %s: %w", userID, err)
	}
	return pgCollectRows(rows, scanExternalAccountRow, "scan external account row", "iterate external accounts")
}

// FindByProviderAndExternalID resolves an account by its provider-side
// id. Used by webhook routing — Strava's payload carries owner_id (the
// athlete-side id) and we map it to our internal account via this query.
//
// Backed by the UNIQUE constraint on (provider, provider_account_id)
// from migration 4 — single index seek.
func (r *ExternalAccountRepo) FindByProviderAndExternalID(
	ctx context.Context,
	provider, externalID, pool string,
) (domain.ExternalAccount, error) {
	db := dbtx(ctx, r.pool)
	row := db.QueryRow(ctx,
		`SELECT `+externalAccountColumns+`
		   FROM external_accounts
		  WHERE provider = $1
		    AND provider_account_id = $2
		    AND ($3 = '' OR assigned_worker_name = $3 OR assigned_worker_name IS NULL)
		  ORDER BY assigned_worker_name IS NULL
		  LIMIT 1`,
		provider, externalID, pool,
	)
	a, err := scanExternalAccountRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ExternalAccount{}, fmt.Errorf(
			"find external account %s/%s: %w", provider, externalID, domain.ErrNotFound)
	}
	if err != nil {
		return domain.ExternalAccount{}, fmt.Errorf(
			"find external account %s/%s: %w", provider, externalID, err)
	}
	return a, nil
}

// SetAutoImport toggles automatic imports for one account (#93). When false,
// the reconcile scheduler skips it and the webhook account-lookup refuses it.
func (r *ExternalAccountRepo) SetAutoImport(
	ctx context.Context,
	id domain.ExternalAccountID,
	enabled bool,
) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE external_accounts SET auto_import_enabled = $2, updated_at = now() WHERE id = $1`,
		id.UUID(), enabled)
	if err != nil {
		return fmt.Errorf("set auto-import: %w", err)
	}
	return nil
}

// SetAssignedWorker pins the account to a named worker pool; "" unpins.
func (r *ExternalAccountRepo) SetAssignedWorker(ctx context.Context, id domain.ExternalAccountID, workerName string) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE external_accounts SET assigned_worker_name = NULLIF($2, ''), updated_at = now() WHERE id = $1`,
		id.UUID(), workerName)
	if isUniqueViolation(err) {
		return fmt.Errorf("set assigned worker: %w", domain.ErrPoolTaken)
	}
	if err != nil {
		return fmt.Errorf("set assigned worker: %w", err)
	}
	return nil
}

// RegisterWebhookSubscription records the provider push-subscription →
// connection mapping (#50) on the account's own webhook_subscription_id column
// (per-connection, so it disambiguates where provider_account_id cannot).
func (r *ExternalAccountRepo) RegisterWebhookSubscription(
	ctx context.Context,
	provider, subscriptionID string,
	accountID domain.ExternalAccountID,
) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE external_accounts
		    SET webhook_subscription_id = $1,
		        webhook_subscribed = true,
		        webhook_subscribed_at = COALESCE(webhook_subscribed_at, now()),
		        updated_at = now()
		  WHERE id = $2 AND provider = $3`,
		subscriptionID, accountID.UUID(), provider)
	if err != nil {
		return fmt.Errorf("register webhook subscription: %w", err)
	}
	return nil
}

// FindBySubscription resolves the exact account behind a provider push
// subscription — unambiguous where FindByProviderAndExternalID is not (a
// per-user-OAuth connection has its own subscription_id).
func (r *ExternalAccountRepo) FindBySubscription(
	ctx context.Context,
	provider, subscriptionID, pool string,
) (domain.ExternalAccount, error) {
	db := dbtx(ctx, r.pool)
	row := db.QueryRow(ctx,
		`SELECT `+externalAccountColumns+`
		   FROM external_accounts
		  WHERE provider = $1 AND webhook_subscription_id = $2
		    AND ($3 = '' OR assigned_worker_name = $3 OR assigned_worker_name IS NULL)
		  ORDER BY assigned_worker_name IS NULL
		  LIMIT 1`,
		provider, subscriptionID, pool)
	a, err := scanExternalAccountRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ExternalAccount{}, fmt.Errorf(
			"find by subscription %s/%s: %w", provider, subscriptionID, domain.ErrNotFound)
	}
	if err != nil {
		return domain.ExternalAccount{}, fmt.Errorf(
			"find by subscription %s/%s: %w", provider, subscriptionID, err)
	}
	return a, nil
}

// ListAccountsDueForReconcile returns accounts whose next reconcile is
// due. See the port-level docstring for the schedule semantics.
//
// SQL semantics:
//
//  1. status filter: 'active' always eligible; 'rate_limited' eligible
//     only if both rate-limit windows have reset (rate_limit jsonb fields
//     are checked against `now`).
//  2. cadence filter:
//     webhook_subscribed = true   →  last_sync_at < now - webhook_drift
//     webhook_subscribed = false  →  last_sync_at < now - polling_interval
//     NULL last_sync_at always counts as "due" (never reconciled).
//  3. ORDER BY last_sync_at NULLS FIRST so initial-backfill accounts
//     and ones that haven't synced in the longest time get processed first.
//  4. LIMIT batchSize bounds the per-tick load.
//
// The pollingInterval and webhookDriftInterval are bound at repo
// construction (from SchedulerConfig). They are passed as Postgres
// interval literals via parameter expansion — pgx encodes time.Duration
// as INTERVAL automatically.
func (r *ExternalAccountRepo) ListAccountsDueForReconcile(
	ctx context.Context,
	now time.Time,
	batchSize int,
) ([]domain.ExternalAccount, error) {
	if batchSize <= 0 {
		batchSize = 100
	}
	db := dbtx(ctx, r.pool)

	rows, err := db.Query(ctx,
		`SELECT `+externalAccountColumns+`
		   FROM external_accounts
		  WHERE auto_import_enabled = true
		  AND (
		      status = 'active'
		      OR (
		          status = 'rate_limited'
		          AND (rate_limit IS NULL
		               OR (
		                   (rate_limit->>'ShortWindowResetAt')::timestamptz < $1
		                   AND (rate_limit->>'LongWindowResetAt')::timestamptz < $1
		               )
		          )
		      )
		  )
		  AND (
		      last_sync_at IS NULL
		      OR (
		          webhook_subscribed = true  AND last_sync_at < $1 - $2::interval
		      )
		      OR (
		          webhook_subscribed = false AND last_sync_at < $1 - COALESCE(
		              NULLIF(config->>'poll_interval_seconds', '')::int * interval '1 second',
		              $3::interval
		          )
		      )
		  )
		  ORDER BY last_sync_at NULLS FIRST, id ASC
		  LIMIT $4`,
		now,
		r.webhookDriftInterval,
		r.pollingInterval,
		batchSize,
	)
	if err != nil {
		return nil, fmt.Errorf("list accounts due for reconcile: %w", err)
	}
	return pgCollectRows(rows, scanExternalAccountRow, "scan due-account row", "iterate due accounts")
}

// ListKnownExternalIDs returns external_ids of sources already imported for
// the account with start_time >= since. Per-account row counts inside the
// reconcile window are small, so the ::timestamptz cast scan is fine.
func (r *ExternalAccountRepo) ListKnownExternalIDs(
	ctx context.Context,
	id domain.ExternalAccountID,
	since time.Time,
	limit int,
) ([]string, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT external_id
		   FROM activity_sources
		  WHERE external_account_id = $1
		    AND (parsed->>'start_time')::timestamptz >= $2
		  ORDER BY (parsed->>'start_time')::timestamptz DESC
		  LIMIT $3`,
		id.UUID(), since, limit)
	if err != nil {
		return nil, fmt.Errorf("list known external ids: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var extID string
		if err := rows.Scan(&extID); err != nil {
			return nil, fmt.Errorf("scan known external id: %w", err)
		}
		out = append(out, extID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate known external ids: %w", err)
	}
	return out, nil
}

// UpdateAccountConfig shallow-merges the patch into the account's config jsonb.
func (r *ExternalAccountRepo) UpdateAccountConfig(
	ctx context.Context,
	id domain.ExternalAccountID,
	patch map[string]any,
) error {
	if len(patch) == 0 {
		return nil
	}
	b, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal config patch: %w", err)
	}
	_, err = r.pool.Exec(ctx,
		`UPDATE external_accounts
		    SET config = COALESCE(config, '{}'::jsonb) || $2::jsonb,
		        updated_at = now()
		  WHERE id = $1`,
		id.UUID(), b)
	if err != nil {
		return fmt.Errorf("update account config %s: %w", id, err)
	}
	return nil
}

// UpdateSyncWatermark advances sync_watermark and stamps last_sync_at
// (always) and last_successful_sync_at (only when succeeded=true).
//
// Watermark monotonicity: the new value must be >= the existing one.
// A reconcile that found no activities passes its `since` value as the
// new watermark; that's fine, it's monotone. A failed reconcile passes
// the existing value with succeeded=false (or skips this call entirely).
func (r *ExternalAccountRepo) UpdateSyncWatermark(
	ctx context.Context,
	id domain.ExternalAccountID,
	watermark time.Time,
	succeeded bool,
	at time.Time,
) error {
	db := dbtx(ctx, r.pool)

	var successAt any
	if succeeded {
		successAt = at
	}

	// NULLIF filters a zero-value watermark (reconcile on an account with no
	// activities yet) to NULL — GREATEST ignores NULLs, so the stored
	// watermark is kept (or stays NULL) instead of becoming year-0001.
	tag, err := db.Exec(ctx,
		`UPDATE external_accounts
		    SET sync_watermark         = GREATEST(sync_watermark, NULLIF($2::timestamptz, '0001-01-01 00:00:00+00'::timestamptz)),
		        last_sync_at           = $3,
		        last_successful_sync_at = COALESCE($4::timestamptz, last_successful_sync_at)
		  WHERE id = $1`,
		id.UUID(), watermark, at, successAt,
	)
	if err != nil {
		return fmt.Errorf("update sync watermark %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update sync watermark %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

// UpdateStatus flips lifecycle status. Used by reauth-event dispatchers,
// the rate-limit observer, the admin disable command, and the
// worker-offline reconciler.
func (r *ExternalAccountRepo) UpdateStatus(
	ctx context.Context,
	id domain.ExternalAccountID,
	status domain.ExternalAccountStatus,
	reason string,
) error {
	if !status.Valid() {
		return fmt.Errorf("update status %s: invalid status %q", id, status)
	}
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE external_accounts
		    SET status = $2, status_reason = $3
		  WHERE id = $1`,
		id.UUID(), string(status), reason,
	)
	if err != nil {
		return fmt.Errorf("update status %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update status %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

// UpdateRateLimit stores the most recent rate-limit snapshot. Used by
// the worker after every successful provider API call (and explicitly
// after 429s with ForceRefill-equivalent values).
//
// snapshot=nil clears the column — used when the worker has no
// authoritative info (e.g., after a successful manual reset).
func (r *ExternalAccountRepo) UpdateRateLimit(
	ctx context.Context,
	id domain.ExternalAccountID,
	snapshot *domain.RateLimitSnapshot,
) error {
	db := dbtx(ctx, r.pool)

	var jsonValue any
	if snapshot != nil {
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return fmt.Errorf("marshal rate-limit snapshot: %w", err)
		}
		jsonValue = raw
	}

	tag, err := db.Exec(ctx,
		`UPDATE external_accounts
		    SET rate_limit = $2
		  WHERE id = $1`,
		id.UUID(), jsonValue,
	)
	if err != nil {
		return fmt.Errorf("update rate-limit %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update rate-limit %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scan helper
// ---------------------------------------------------------------------------

func scanExternalAccountRow(row rowScanner) (domain.ExternalAccount, error) {
	var (
		a                  domain.ExternalAccount
		id, userID         uuid.UUID
		providerAccountID  *string
		connectionID       *uuid.UUID
		assignedWorkerName *string
		status             string
		statusReason       *string
		configRaw          []byte
		webhookSubID       *string
		rateLimitRaw       []byte
	)
	if err := row.Scan(
		&id, &userID, &a.Provider, &providerAccountID, &connectionID,
		&a.DisplayLabel, &assignedWorkerName,
		&status, &statusReason,
		&configRaw, &a.GrantedScopes,
		&a.WebhookSubscribed, &a.WebhookSubscribedAt, &webhookSubID,
		&rateLimitRaw,
		&a.SyncWatermark, &a.LastSyncAt, &a.LastSuccessfulSyncAt,
		&a.CreatedAt, &a.UpdatedAt,
		&a.AutoImportEnabled,
	); err != nil {
		return domain.ExternalAccount{}, err
	}

	a.ID = domain.ExternalAccountID(id)
	a.UserID = domain.UserID(userID)
	a.Status = domain.ExternalAccountStatus(status)
	if providerAccountID != nil {
		a.ProviderAccountID = *providerAccountID
	}
	if connectionID != nil {
		c := domain.ConnectionID(*connectionID)
		a.ConnectionID = &c
	}
	if statusReason != nil {
		a.StatusReason = *statusReason
	}
	if assignedWorkerName != nil {
		a.AssignedWorkerName = *assignedWorkerName
	}
	if webhookSubID != nil {
		a.WebhookSubscriptionID = *webhookSubID
	}
	if len(configRaw) > 0 {
		a.Config = json.RawMessage(configRaw)
	}
	if len(rateLimitRaw) > 0 {
		var snap domain.RateLimitSnapshot
		if err := json.Unmarshal(rateLimitRaw, &snap); err == nil {
			a.RateLimit = &snap
		}
	}
	return a, nil
}
