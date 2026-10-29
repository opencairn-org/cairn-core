//go:build integration

package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestMetricRepoQueries(t *testing.T) {
	ctx, pool := requirePool(t)
	userID := seedUser(t)
	repo := NewMetricRepo(pool)
	base := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	var ids []domain.MetricID
	rows := []domain.Metric{
		{Key: "weight_kg", Timestamp: base, ValueNumeric: f(80), Provider: "manual"},
		{Key: "weight_kg", Timestamp: base.Add(26 * time.Hour), ValueNumeric: f(79), Provider: "manual"},
		{Key: "weight_kg", Timestamp: base.Add(50 * time.Hour), ValueNumeric: f(78), Provider: "withings", ExternalID: "w1"},
		{Key: "resting_hr_bpm", Timestamp: base.Add(time.Hour), ValueNumeric: f(48), Provider: "manual"},
	}
	for i := range rows {
		rows[i].ID, rows[i].UserID = domain.MetricID(uuid.New()), userID
		ids = append(ids, rows[i].ID)
	}
	if err := repo.SaveMetrics(ctx, rows); err != nil {
		t.Fatal(err)
	}

	list, total, err := repo.ListMetrics(ctx, userID, domain.MetricListFilter{Keys: []string{"weight_kg"}, Start: base.Add(-time.Hour), End: base.Add(72 * time.Hour), Limit: 2})
	if err != nil || total != 3 || len(list) != 2 || *list[0].ValueNumeric != 78 {
		t.Fatalf("list desc: %v %d %+v", err, total, list)
	}
	list, _, err = repo.ListMetrics(ctx, userID, domain.MetricListFilter{Providers: []string{"manual"}, Start: base.Add(-time.Hour), End: base.Add(72 * time.Hour), Ascending: true, Limit: 10})
	if err != nil || len(list) != 3 || *list[0].ValueNumeric != 80 {
		t.Fatalf("list asc by provider: %v %+v", err, list)
	}

	latest, err := repo.LatestMetrics(ctx, userID, nil, base.Add(72*time.Hour))
	if err != nil || len(latest) != 2 {
		t.Fatalf("latest all: %v %+v", err, latest)
	}
	latest, err = repo.LatestMetrics(ctx, userID, []string{"weight_kg"}, base.Add(30*time.Hour))
	if err != nil || len(latest) != 1 || *latest[0].ValueNumeric != 79 {
		t.Fatalf("latest as-of: %v %+v", err, latest)
	}

	raw, err := repo.QueryMetricSeries(ctx, userID, domain.MetricSeriesQuery{Key: "weight_kg", Start: base.Add(-time.Hour), End: base.Add(72 * time.Hour)})
	if err != nil || len(raw) != 3 || raw[0].Value != 80 || raw[2].Samples != 1 {
		t.Fatalf("raw series: %v %+v", err, raw)
	}
	daily, err := repo.QueryMetricSeries(ctx, userID, domain.MetricSeriesQuery{Key: "weight_kg", Start: base.Add(-time.Hour), End: base.Add(72 * time.Hour),
		Bucket: domain.MetricBucketDay, Aggregation: domain.MetricAggLast})
	if err != nil || len(daily) != 3 || daily[0].Value != 80 || daily[0].Samples != 1 {
		t.Fatalf("daily last: %v %+v", err, daily)
	}
	weekly, err := repo.QueryMetricSeries(ctx, userID, domain.MetricSeriesQuery{Key: "weight_kg", Start: base.Add(-time.Hour), End: base.Add(72 * time.Hour),
		Bucket: domain.MetricBucketWeek, Aggregation: domain.MetricAggAvg, Providers: []string{"manual"}})
	if err != nil || len(weekly) != 1 || weekly[0].Value != 79.5 || weekly[0].Samples != 2 {
		t.Fatalf("weekly avg: %v %+v", err, weekly)
	}
	count, err := repo.QueryMetricSeries(ctx, userID, domain.MetricSeriesQuery{Key: "weight_kg", Start: base.Add(-time.Hour), End: base.Add(72 * time.Hour),
		Bucket: domain.MetricBucketMonth, Aggregation: domain.MetricAggCount})
	if err != nil || len(count) != 1 || count[0].Value != 3 {
		t.Fatalf("monthly count: %v %+v", err, count)
	}

	got, err := repo.GetMetric(ctx, userID, ids[0])
	if err != nil || got.Key != "weight_kg" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := repo.GetMetric(ctx, domain.UserID(uuid.New()), ids[0]); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign get: %v", err)
	}
	found, err := repo.FindMetricByNaturalKey(ctx, userID, "weight_kg", base, "manual")
	if err != nil || found.ID != ids[0] {
		t.Fatalf("natural key: %v %+v", err, found)
	}
	if _, err := repo.FindMetricByNaturalKey(ctx, userID, "weight_kg", base, "garmin"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("natural key miss: %v", err)
	}
	got.ValueNumeric, got.Tags, got.Notes = f(81), []string{"edited"}, "note"
	if err := repo.UpdateMetric(ctx, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := repo.GetMetric(ctx, userID, ids[0]); *again.ValueNumeric != 81 || again.Notes != "note" || len(again.Tags) != 1 {
		t.Fatalf("update: %+v", again)
	}
	got.ID = domain.MetricID(uuid.New())
	if err := repo.UpdateMetric(ctx, got); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	if err := repo.DeleteMetric(ctx, userID, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteMetric(ctx, userID, ids[0]); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}
