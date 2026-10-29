package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// MetricRepo persists and reads `metrics` hypertable rows.
//
// Upsert semantics: rows with non-empty ExternalID conflict-resolve via
// the (user_id, key, ts, provider, external_id) unique index from
// migration 9. Computed metrics (provider = cairn) always set
// ExternalID = "computed:<timestamp>" so reruns of the same compute
// pass overwrite cleanly. Manual entries (ExternalID empty) never
// dedup — each call inserts a fresh row.
type MetricRepo interface {
	// SaveMetrics upserts a batch by the natural key described above.
	// Empty input is a no-op.
	SaveMetrics(ctx context.Context, metrics []domain.Metric) error

	// ListMetricsForUser returns metrics for the user within [start, end],
	// optionally filtered by key (empty matches all). Ordered by ts ASC.
	// The window is inclusive on both ends.
	ListMetricsForUser(
		ctx context.Context,
		userID domain.UserID,
		key string,
		start, end time.Time,
	) ([]domain.Metric, error)

	// DeleteComputedForUser removes every metrics row whose provider is
	// domain.MetricProviderComputed and whose key matches one of the
	// supplied keys, within [start, end]. Used by ComputeTrainingLoad
	// before rewriting the user's rollups so stale rows from prior
	// compute windows don't linger.
	DeleteComputedForUser(
		ctx context.Context,
		userID domain.UserID,
		keys []string,
		start, end time.Time,
	) error

	// ListMetrics pages the user's rows by the filter, newest first unless
	// Ascending; total is the unpaged match count.
	ListMetrics(ctx context.Context, userID domain.UserID, f domain.MetricListFilter) (rows []domain.Metric, total int, err error)
	// LatestMetrics returns the newest row per key at or before asOf; empty
	// keys means every key the user has.
	LatestMetrics(ctx context.Context, userID domain.UserID, keys []string, asOf time.Time) ([]domain.Metric, error)
	// QueryMetricSeries folds numeric samples of one key into buckets (raw
	// points without a bucket), ascending by time.
	QueryMetricSeries(ctx context.Context, userID domain.UserID, q domain.MetricSeriesQuery) ([]domain.MetricSeriesPoint, error)
	// GetMetric reads one row scoped to its owner; domain.ErrNotFound on miss.
	GetMetric(ctx context.Context, userID domain.UserID, id domain.MetricID) (domain.Metric, error)
	// FindMetricByNaturalKey resolves the row of (key, ts, provider) for a
	// natural-key upsert of manual entries; domain.ErrNotFound on miss.
	FindMetricByNaturalKey(ctx context.Context, userID domain.UserID, key string, ts time.Time, provider string) (domain.Metric, error)
	// UpdateMetric rewrites the mutable columns of an existing row by id.
	UpdateMetric(ctx context.Context, m domain.Metric) error
	// DeleteMetric removes one row scoped to its owner; domain.ErrNotFound on miss.
	DeleteMetric(ctx context.Context, userID domain.UserID, id domain.MetricID) error
}
