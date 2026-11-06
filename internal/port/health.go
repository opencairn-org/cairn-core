package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/domain/health"
)

// HealthRepo persists raw per-provider health samples (HRV, Sleep, Weight, …).
// The merged daily view is derived on read via health.MergeDaily, so the raw
// archive stays the source of truth.
type HealthRepo interface {
	// SaveSamples upserts raw samples on the natural key (user, type, ts,
	// provider, external_id). Written by worker METRIC imports (metrics.Store.Import).
	SaveSamples(ctx context.Context, userID domain.UserID, accountID *domain.ExternalAccountID, samples []health.Sample) error

	// ListSamples returns raw samples for one user + data type in [from, to).
	ListSamples(ctx context.Context, userID domain.UserID, dataType capability.DataType, from, to time.Time) ([]health.Sample, error)
}
