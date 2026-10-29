package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// InstanceSettingsRepo persists the single instance_settings row.
type InstanceSettingsRepo interface {
	// Get returns the row, creating the defaults when none exists yet.
	Get(ctx context.Context) (domain.InstanceSettings, error)
	// Save writes the editable columns.
	Save(ctx context.Context, s domain.InstanceSettings) error
	// MirrorEnv records the operator's email and storage configuration so
	// the admin UI can show it without reading the environment.
	MirrorEnv(ctx context.Context, emailAdapter string, emailConfigured bool, emailFrom, storageAdapter string, storageConfigured bool) error
}
