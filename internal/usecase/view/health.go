package view

import (
	"context"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/domain/health"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Health reads the merged daily health series (MetricsService.ListHealthSeries,
// GET /api/health).
type Health struct {
	health port.HealthRepo
	now    func() time.Time
}

// NewHealth wires the repo; nil → ErrUnavailable.
func NewHealth(h port.HealthRepo, now func() time.Time) *Health {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Health{health: h, now: now}
}

// Health window bounds.
const (
	HealthDefaultDays = 90
	HealthMaxDays     = 3650
)

// Series returns the per-day merged samples of one time-series data type
// over the last days. The provider cascade is the AnyProvider default.
func (h *Health) Series(ctx context.Context, userID domain.UserID, dataType string, days int) ([]health.MergedSample, error) {
	if h.health == nil {
		return nil, ErrUnavailable
	}
	dt := capability.DataType(dataType)
	if dt.CategoryOf() != capability.CategoryTimeSeries {
		return nil, port.NewActionError(port.ActionErrorInvalid, "type must be a time-series health data type")
	}
	if days <= 0 || days > HealthMaxDays {
		days = HealthDefaultDays
	}
	to := h.now().UTC()
	samples, err := h.health.ListSamples(ctx, userID, dt, to.AddDate(0, 0, -days), to)
	if err != nil {
		return nil, fmt.Errorf("list health samples: %w", err)
	}
	return health.MergeDaily(samples, nil), nil
}
