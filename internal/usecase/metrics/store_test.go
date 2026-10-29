package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeRepo struct {
	port.MetricRepo
	rows map[domain.MetricID]domain.Metric
}

func (f *fakeRepo) SaveMetrics(_ context.Context, ms []domain.Metric) error {
	for _, m := range ms {
		f.rows[m.ID] = m
	}
	return nil
}
func (f *fakeRepo) GetMetric(_ context.Context, user domain.UserID, id domain.MetricID) (domain.Metric, error) {
	m, ok := f.rows[id]
	if !ok || m.UserID != user {
		return domain.Metric{}, domain.ErrNotFound
	}
	return m, nil
}
func (f *fakeRepo) FindMetricByNaturalKey(_ context.Context, user domain.UserID, key string, ts time.Time, provider string) (domain.Metric, error) {
	for _, m := range f.rows {
		if m.UserID == user && m.Key == key && m.Timestamp.Equal(ts) && m.Provider == provider {
			return m, nil
		}
	}
	return domain.Metric{}, domain.ErrNotFound
}
func (f *fakeRepo) UpdateMetric(_ context.Context, m domain.Metric) error {
	if _, ok := f.rows[m.ID]; !ok {
		return domain.ErrNotFound
	}
	f.rows[m.ID] = m
	return nil
}
func (f *fakeRepo) DeleteMetric(_ context.Context, user domain.UserID, id domain.MetricID) error {
	if m, ok := f.rows[id]; !ok || m.UserID != user {
		return domain.ErrNotFound
	}
	delete(f.rows, id)
	return nil
}
func (f *fakeRepo) LatestMetrics(_ context.Context, user domain.UserID, keys []string, asOf time.Time) ([]domain.Metric, error) {
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	latest := map[string]domain.Metric{}
	for _, m := range f.rows {
		if m.UserID != user || m.Timestamp.After(asOf) || (len(want) > 0 && !want[m.Key]) {
			continue
		}
		if cur, ok := latest[m.Key]; !ok || m.Timestamp.After(cur.Timestamp) {
			latest[m.Key] = m
		}
	}
	out := make([]domain.Metric, 0, len(latest))
	for _, m := range latest {
		out = append(out, m)
	}
	return out, nil
}
func (f *fakeRepo) QueryMetricSeries(_ context.Context, user domain.UserID, q domain.MetricSeriesQuery) ([]domain.MetricSeriesPoint, error) {
	var out []domain.MetricSeriesPoint
	for _, m := range f.rows {
		if m.UserID == user && m.Key == q.Key && m.ValueNumeric != nil {
			out = append(out, domain.MetricSeriesPoint{Ts: m.Timestamp, Value: *m.ValueNumeric, Samples: 1})
		}
	}
	return out, nil
}
func (f *fakeRepo) ListMetrics(_ context.Context, user domain.UserID, filter domain.MetricListFilter) ([]domain.Metric, int, error) {
	var out []domain.Metric
	for _, m := range f.rows {
		if m.UserID == user {
			out = append(out, m)
		}
	}
	return out, len(out), nil
}

type fakeAthlete struct {
	port.AthleteProfileRepo
	entries []domain.AthleteMetricEntry
}

func (f *fakeAthlete) ListEntries(context.Context, domain.UserID) ([]domain.AthleteMetricEntry, error) {
	return f.entries, nil
}

func kindOf(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

func f64(v float64) *float64 { return &v }

func TestStoreUpsertEditDelete(t *testing.T) {
	ctx := t.Context()
	user := domain.UserID(uuid.New())
	repo := &fakeRepo{rows: map[domain.MetricID]domain.Metric{}}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	s := &Store{Repo: repo, Now: func() time.Time { return now }}

	if _, _, err := (&Store{}).Upsert(ctx, user, UpsertInput{}); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("unwired: %v", err)
	}
	if _, _, err := s.Upsert(ctx, user, UpsertInput{Key: "Weight", Ts: now, ValueNumeric: f64(1)}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("bad key: %v", err)
	}
	if _, _, err := s.Upsert(ctx, user, UpsertInput{Key: "weight_kg", Ts: now}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("no value: %v", err)
	}
	if _, _, err := s.Upsert(ctx, user, UpsertInput{Key: "weight_kg", Ts: now, ValueStruct: []byte(`{"a":1}`)}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("shape: %v", err)
	}
	m, created, err := s.Upsert(ctx, user, UpsertInput{Key: "weight_kg", Ts: now, ValueNumeric: f64(78.2), Tags: []string{"morning"}})
	if err != nil || !created || m.Provider != domain.MetricProviderManual || m.ID == (domain.MetricID{}) {
		t.Fatalf("create: %v %v %+v", err, created, m)
	}
	// Natural-key dedup reuses the row; a plain upsert adds another.
	again, created, err := s.Upsert(ctx, user, UpsertInput{Key: "weight_kg", Ts: now, ValueNumeric: f64(78.5), DedupByNaturalKey: true})
	if err != nil || created || again.ID != m.ID || *again.ValueNumeric != 78.5 {
		t.Fatalf("dedup: %v %v %+v", err, created, again)
	}
	if _, created, _ := s.Upsert(ctx, user, UpsertInput{Key: "weight_kg", Ts: now, ValueNumeric: f64(1)}); !created || len(repo.rows) != 2 {
		t.Fatalf("duplicate manual rows allowed: %d", len(repo.rows))
	}
	if _, _, err := s.Upsert(ctx, user, UpsertInput{ID: &m.ID, Key: "steps", Ts: now, ValueNumeric: f64(1)}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("key change: %v", err)
	}
	other := domain.MetricID(uuid.New())
	if _, _, err := s.Upsert(ctx, user, UpsertInput{ID: &other, Key: "weight_kg", Ts: now, ValueNumeric: f64(1)}); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("edit missing: %v", err)
	}
	if err := s.Delete(ctx, domain.UserID(uuid.New()), m.ID); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("delete foreign: %v", err)
	}
	if err := s.Delete(ctx, user, m.ID); err != nil || len(repo.rows) != 1 {
		t.Fatalf("delete: %v", err)
	}
}

func TestStoreBulkLatestSeriesContext(t *testing.T) {
	ctx := t.Context()
	user := domain.UserID(uuid.New())
	repo := &fakeRepo{rows: map[domain.MetricID]domain.Metric{}}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	athlete := &fakeAthlete{entries: []domain.AthleteMetricEntry{{Key: domain.AthleteFTPWatts, EffectiveDate: now.AddDate(0, -1, 0), Value: 250},
		{Key: domain.AthleteWeightKg, EffectiveDate: now.AddDate(0, -1, 0), Value: 70}}}
	s := &Store{Repo: repo, Athlete: athlete, Now: func() time.Time { return now }}

	rows := []UpsertInput{
		{Key: "weight_kg", Ts: now.AddDate(0, 0, -2), ValueNumeric: f64(80)},
		{Key: "bad key", Ts: now, ValueNumeric: f64(1)},
		{Key: "weight_kg", Ts: now.AddDate(0, 0, -1), ValueNumeric: f64(79)},
		{Key: "resting_hr_bpm", Ts: now, ValueNumeric: f64(48)},
	}
	res, err := s.BulkUpsert(ctx, user, rows, false)
	if err != nil || res.Created != 1 || res.Skipped != 2 || len(res.Errors) != 1 || res.Errors[0].Row != 1 || res.Errors[0].Code != "invalid" {
		t.Fatalf("bulk stop: %v %+v", err, res)
	}
	res, err = s.BulkUpsert(ctx, user, rows, true)
	if err != nil || res.Created != 3 || res.Skipped != 0 || len(res.Errors) != 1 {
		t.Fatalf("bulk continue: %v %+v", err, res)
	}
	if _, err := s.BulkUpsert(ctx, user, make([]UpsertInput, MaxBulkRows+1), true); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("bulk cap: %v", err)
	}

	latest, err := s.Latest(ctx, user, []string{"weight_kg"}, nil)
	if err != nil || len(latest) != 1 || *latest["weight_kg"].ValueNumeric != 79 {
		t.Fatalf("latest: %v %+v", err, latest)
	}
	earlier := now.AddDate(0, 0, -2)
	if latest, _ = s.Latest(ctx, user, nil, &earlier); *latest["weight_kg"].ValueNumeric != 80 || len(latest) != 1 {
		t.Fatalf("latest as-of: %+v", latest)
	}

	if _, err := s.Series(ctx, user, domain.MetricSeriesQuery{}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("series no key: %v", err)
	}
	if _, err := s.Series(ctx, user, domain.MetricSeriesQuery{Key: "weight_kg", Bucket: "fortnight"}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("series bad bucket: %v", err)
	}
	pts, err := s.Series(ctx, user, domain.MetricSeriesQuery{Key: "weight_kg", MaxPoints: 1})
	if err != nil || len(pts) != 1 {
		t.Fatalf("series capped: %v %+v", err, pts)
	}

	// Stored measurements win; the athlete profile fills what is missing.
	c, err := s.Context(ctx, user, nil)
	if err != nil || *c["weight_kg"].ValueNumeric != 79 || *c["ftp_cycling_w"].ValueNumeric != 250 || *c["resting_hr_bpm"].ValueNumeric != 48 {
		t.Fatalf("context: %v %+v", err, c)
	}
	if _, ok := c["lthr_running_bpm"]; ok {
		t.Fatalf("no threshold hr expected: %+v", c)
	}
}

func TestDownsample(t *testing.T) {
	pts := make([]domain.MetricSeriesPoint, 10)
	for i := range pts {
		pts[i].Value = float64(i)
	}
	got := domain.Downsample(pts, 4)
	if len(got) != 4 || got[0].Value != 0 || got[3].Value != 9 {
		t.Fatalf("downsample: %+v", got)
	}
	if len(domain.Downsample(pts, 0)) != 10 || len(domain.Downsample(pts, 1)) != 1 {
		t.Fatal("edge cases")
	}
}
