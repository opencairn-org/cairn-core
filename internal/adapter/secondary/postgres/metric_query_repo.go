package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ListMetrics pages the filtered rows and counts the unpaged match.
func (r *MetricRepo) ListMetrics(ctx context.Context, userID domain.UserID, f domain.MetricListFilter) ([]domain.Metric, int, error) {
	db := dbtx(ctx, r.pool)
	where, args := metricFilterSQL(userID, f)
	var total int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM metrics `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count metrics: %w", err)
	}
	order := "DESC"
	if f.Ascending {
		order = "ASC"
	}
	args = append(args, f.Limit, f.Offset)
	rows, err := db.Query(ctx, fmt.Sprintf(`SELECT %s FROM metrics %s ORDER BY ts %s, id %s LIMIT $%d OFFSET $%d`,
		metricColumns, where, order, order, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list metrics: %w", err)
	}
	out, err := pgCollectRows(rows, scanMetricRow, "scan metric row", "iterate metrics")
	return out, total, err
}

// metricFilterSQL renders the WHERE clause of a listing.
func metricFilterSQL(userID domain.UserID, f domain.MetricListFilter) (string, []any) {
	args := []any{userID.UUID(), f.Start, f.End}
	parts := []string{"user_id = $1", "ts BETWEEN $2 AND $3"}
	add := func(clause string, v any) {
		args = append(args, v)
		parts = append(parts, fmt.Sprintf(clause, len(args)))
	}
	if len(f.Keys) > 0 {
		add("key = ANY($%d)", f.Keys)
	}
	if len(f.Providers) > 0 {
		add("provider = ANY($%d)", f.Providers)
	}
	if f.ExternalAccountID != nil {
		add("external_account_id = $%d", f.ExternalAccountID.UUID())
	}
	if f.ActivityID != nil {
		add("activity_id = $%d", f.ActivityID.UUID())
	}
	return "WHERE " + strings.Join(parts, " AND "), args
}

// LatestMetrics picks the newest row per key at or before asOf.
func (r *MetricRepo) LatestMetrics(ctx context.Context, userID domain.UserID, keys []string, asOf time.Time) ([]domain.Metric, error) {
	db := dbtx(ctx, r.pool)
	args := []any{userID.UUID(), asOf}
	q := `SELECT DISTINCT ON (key) ` + metricColumns + ` FROM metrics WHERE user_id = $1 AND ts <= $2`
	if len(keys) > 0 {
		args = append(args, keys)
		q += ` AND key = ANY($3)`
	}
	rows, err := db.Query(ctx, q+` ORDER BY key, ts DESC, id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("latest metrics: %w", err)
	}
	return pgCollectRows(rows, scanMetricRow, "scan metric row", "iterate metrics")
}

// metricAggSQL maps an aggregation onto the TimescaleDB expression.
var metricAggSQL = map[domain.MetricAggregation]string{
	domain.MetricAggAvg: "avg(value_numeric)", domain.MetricAggMin: "min(value_numeric)", domain.MetricAggMax: "max(value_numeric)",
	domain.MetricAggSum: "sum(value_numeric)", domain.MetricAggLast: "last(value_numeric, ts)", domain.MetricAggFirst: "first(value_numeric, ts)",
	domain.MetricAggCount: "count(value_numeric)::double precision",
}

// QueryMetricSeries returns raw points, or one aggregated point per bucket.
func (r *MetricRepo) QueryMetricSeries(ctx context.Context, userID domain.UserID, q domain.MetricSeriesQuery) ([]domain.MetricSeriesPoint, error) {
	db := dbtx(ctx, r.pool)
	args := []any{userID.UUID(), q.Key, q.Start, q.End}
	where := `WHERE user_id = $1 AND key = $2 AND ts BETWEEN $3 AND $4 AND value_numeric IS NOT NULL`
	if len(q.Providers) > 0 {
		args = append(args, q.Providers)
		where += ` AND provider = ANY($5)`
	}
	sql := `SELECT ts, value_numeric, 1 FROM metrics ` + where + ` ORDER BY ts`
	if q.Bucket != domain.MetricBucketNone {
		agg, ok := metricAggSQL[q.Aggregation]
		if !ok {
			return nil, domain.ErrMetricAggregation
		}
		args = append(args, q.Bucket.Duration())
		bucket := fmt.Sprintf("time_bucket($%d::interval, ts)", len(args))
		sql = fmt.Sprintf(`SELECT %s AS bucket, %s, count(*) FROM metrics %s GROUP BY bucket ORDER BY bucket`, bucket, agg, where)
	}
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("metric series: %w", err)
	}
	defer rows.Close()
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.MetricSeriesPoint, error) {
		var p domain.MetricSeriesPoint
		err := row.Scan(&p.Ts, &p.Value, &p.Samples)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan metric series: %w", err)
	}
	return out, nil
}

func (r *MetricRepo) GetMetric(ctx context.Context, userID domain.UserID, id domain.MetricID) (domain.Metric, error) {
	row := dbtx(ctx, r.pool).QueryRow(ctx, `SELECT `+metricColumns+` FROM metrics WHERE user_id = $1 AND id = $2`, userID.UUID(), id.UUID())
	return scanOneMetric(row, "get metric")
}

func (r *MetricRepo) FindMetricByNaturalKey(ctx context.Context, userID domain.UserID, key string, ts time.Time, provider string) (domain.Metric, error) {
	row := dbtx(ctx, r.pool).QueryRow(ctx, `SELECT `+metricColumns+` FROM metrics
		WHERE user_id = $1 AND key = $2 AND ts = $3 AND provider = $4 ORDER BY id LIMIT 1`, userID.UUID(), key, ts, provider)
	return scanOneMetric(row, "find metric")
}

func scanOneMetric(row rowScanner, what string) (domain.Metric, error) {
	m, err := scanMetricRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Metric{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Metric{}, fmt.Errorf("%s: %w", what, err)
	}
	return m, nil
}

// UpdateMetric rewrites the value, period, tags and notes of one row.
func (r *MetricRepo) UpdateMetric(ctx context.Context, m domain.Metric) error {
	var structParam any
	if len(m.ValueStruct) > 0 {
		structParam = []byte(m.ValueStruct)
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	tag, err := dbtx(ctx, r.pool).Exec(ctx, `UPDATE metrics SET period_seconds = $3, value_numeric = $4, value_struct = $5, tags = $6, notes = $7
		WHERE user_id = $1 AND id = $2`, m.UserID.UUID(), m.ID.UUID(), m.PeriodSeconds, m.ValueNumeric, structParam, tags, m.Notes)
	if err != nil {
		return fmt.Errorf("update metric: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *MetricRepo) DeleteMetric(ctx context.Context, userID domain.UserID, id domain.MetricID) error {
	tag, err := dbtx(ctx, r.pool).Exec(ctx, `DELETE FROM metrics WHERE user_id = $1 AND id = $2`, userID.UUID(), id.UUID())
	if err != nil {
		return fmt.Errorf("delete metric: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
