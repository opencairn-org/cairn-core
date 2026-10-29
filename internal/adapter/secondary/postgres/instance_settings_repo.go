package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// InstanceSettingsRepo implements port.InstanceSettingsRepo over the
// single-row instance_settings table (merge defaults live in the same row
// but are owned by UserSettingsRepo).
type InstanceSettingsRepo struct {
	pool *pgxpool.Pool
}

var _ port.InstanceSettingsRepo = (*InstanceSettingsRepo)(nil)

func NewInstanceSettingsRepo(pool *pgxpool.Pool) *InstanceSettingsRepo {
	return &InstanceSettingsRepo{pool: pool}
}

const instanceSettingsColumns = `instance_name, instance_description, COALESCE(support_email::text, ''), instance_url,
	COALESCE(logo_url, ''), primary_color, default_locale, default_timezone, default_units,
	registration_open, require_invite, allowed_email_domains,
	email_adapter, email_configured, COALESCE(email_from_address::text, ''), storage_adapter, storage_configured,
	per_user_storage_quota_bytes, per_user_max_external_accounts, updated_at`

func (r *InstanceSettingsRepo) Get(ctx context.Context) (domain.InstanceSettings, error) {
	db := dbtx(ctx, r.pool)
	if _, err := db.Exec(ctx, `INSERT INTO instance_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING`); err != nil {
		return domain.InstanceSettings{}, fmt.Errorf("ensure instance settings: %w", err)
	}
	var (
		s     domain.InstanceSettings
		units string
	)
	err := db.QueryRow(ctx, `SELECT `+instanceSettingsColumns+` FROM instance_settings WHERE id = 1`).Scan(
		&s.Name, &s.Description, &s.SupportEmail, &s.URL, &s.LogoURL, &s.PrimaryColor, &s.DefaultLocale, &s.DefaultTimezone, &units,
		&s.RegistrationOpen, &s.RequireInvite, &s.AllowedEmailDomains,
		&s.EmailAdapter, &s.EmailConfigured, &s.EmailFromAddress, &s.StorageAdapter, &s.StorageConfigured,
		&s.PerUserStorageQuotaBytes, &s.PerUserMaxExternalAccounts, &s.UpdatedAt)
	if err != nil {
		return domain.InstanceSettings{}, fmt.Errorf("read instance settings: %w", err)
	}
	s.DefaultUnits = domain.UserUnits(units)
	if s.AllowedEmailDomains == nil {
		s.AllowedEmailDomains = []string{}
	}
	return s, nil
}

func (r *InstanceSettingsRepo) Save(ctx context.Context, s domain.InstanceSettings) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE instance_settings
		    SET instance_name = $1, instance_description = $2, support_email = NULLIF($3, '')::citext, instance_url = $4,
		        logo_url = NULLIF($5, ''), primary_color = $6, default_locale = $7, default_timezone = $8, default_units = $9,
		        registration_open = $10, require_invite = $11, allowed_email_domains = $12,
		        per_user_storage_quota_bytes = $13, per_user_max_external_accounts = $14, updated_at = now()
		  WHERE id = 1`,
		s.Name, s.Description, s.SupportEmail, s.URL, s.LogoURL, s.PrimaryColor, s.DefaultLocale, s.DefaultTimezone, string(s.DefaultUnits),
		s.RegistrationOpen, s.RequireInvite, s.AllowedEmailDomains, s.PerUserStorageQuotaBytes, s.PerUserMaxExternalAccounts)
	if err != nil {
		return fmt.Errorf("save instance settings: %w", err)
	}
	return nil
}

func (r *InstanceSettingsRepo) MirrorEnv(ctx context.Context, emailAdapter string, emailConfigured bool, emailFrom, storageAdapter string, storageConfigured bool) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`INSERT INTO instance_settings (id, email_adapter, email_configured, email_from_address, storage_adapter, storage_configured)
		 VALUES (1, $1, $2, NULLIF($3, '')::citext, $4, $5)
		 ON CONFLICT (id) DO UPDATE SET email_adapter = EXCLUDED.email_adapter, email_configured = EXCLUDED.email_configured,
		     email_from_address = EXCLUDED.email_from_address, storage_adapter = EXCLUDED.storage_adapter, storage_configured = EXCLUDED.storage_configured`,
		emailAdapter, emailConfigured, emailFrom, storageAdapter, storageConfigured)
	if err != nil {
		return fmt.Errorf("mirror env into instance settings: %w", err)
	}
	return nil
}
