package view

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/domain/health"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeMetrics struct {
	port.MetricRepo
	rows []domain.Metric
}

func (f *fakeMetrics) ListMetricsForUser(context.Context, domain.UserID, string, time.Time, time.Time) ([]domain.Metric, error) {
	return f.rows, nil
}

type fakeBestEfforts struct {
	port.BestEffortRepo
	items []domain.BestEffortHistoryItem
}

func (f *fakeBestEfforts) ListBestEffortHistory(context.Context, domain.UserID, string, string, string, int) ([]domain.BestEffortHistoryItem, error) {
	return f.items, nil
}

type fakeGeo struct {
	port.StreamRepo
	tracks map[domain.SourceID][]domain.GeoPoint
}

func (f *fakeGeo) QueryGeoTracks(_ context.Context, ids []domain.SourceID, _ domain.StreamResolution) (map[domain.SourceID][]domain.GeoPoint, error) {
	out := map[domain.SourceID][]domain.GeoPoint{}
	for _, id := range ids {
		out[id] = f.tracks[id]
	}
	return out, nil
}

type fakeHealthRepo struct {
	port.HealthRepo
	samples []health.Sample
}

func (f *fakeHealthRepo) ListSamples(context.Context, domain.UserID, capability.DataType, time.Time, time.Time) ([]health.Sample, error) {
	return f.samples, nil
}

func TestStatsGetAndTrainingLoad(t *testing.T) {
	repo := inmem.NewActivityRepo()
	user := domain.UserID(uuid.New())
	addActivity(repo, user, domain.ActivityTypeRide, vNow.Add(-time.Hour), 50000, domain.PrivacyPrivate)
	addActivity(repo, user, domain.ActivityTypeRun, vNow.Add(-400*24*time.Hour), 10000, domain.PrivacyPrivate)
	v := 42.0
	metrics := &fakeMetrics{rows: []domain.Metric{
		{Key: domain.MetricKeyTrainingLoadCTL, Timestamp: vNow.Add(-24 * time.Hour), ValueNumeric: &v},
		{Key: domain.MetricKeyTrainingLoadTSS, Timestamp: vNow.Add(-24 * time.Hour), ValueNumeric: &v},
		{Key: domain.MetricKeyTrainingLoadCTL, Timestamp: vNow, ValueNumeric: f64p(43.26)},
	}}
	st := NewStats(repo, metrics, nil, nil, func() time.Time { return vNow })
	res, err := st.Get(context.Background(), user)
	if err != nil || res.Totals.Count != 2 || len(res.ByYear) != 2 || len(res.BySport) != 2 {
		t.Fatalf("stats %+v %v", res, err)
	}
	series, err := st.TrainingLoad(context.Background(), user, 0)
	if err != nil || len(series) != 2 || series[0].CTL != 42 || series[0].TSS != 42 || series[1].CTL != 43.3 {
		t.Fatalf("series %+v %v", series, err)
	}
	if _, err := NewStats(repo, nil, nil, nil, nil).TrainingLoad(context.Background(), user, 10); err == nil {
		t.Fatal("no metrics repo must be unavailable")
	}
}

func TestStatsBestEffortHistory(t *testing.T) {
	be := &fakeBestEfforts{items: []domain.BestEffortHistoryItem{{AchievedValue: 300}, {AchievedValue: 280}, {AchievedValue: 290}}}
	st := NewStats(inmem.NewActivityRepo(), nil, be, nil, nil)
	h, err := st.BestEffortHistory(context.Background(), domain.UserID(uuid.New()), "run", "pace", "distance", 5000)
	if err != nil || h.Best != 280 || len(h.Items) != 3 {
		t.Fatalf("pace history %+v %v", h, err)
	}
	h, _ = st.BestEffortHistory(context.Background(), domain.UserID(uuid.New()), "ride", "power", "duration", 1200)
	if h.Best != 300 {
		t.Fatalf("power best %v", h.Best)
	}
	if _, err := st.BestEffortHistory(context.Background(), domain.UserID(uuid.New()), "", "power", "duration", 1200); err == nil {
		t.Fatal("missing type must be invalid")
	}
	if _, err := st.BestEffortHistory(context.Background(), domain.UserID(uuid.New()), "ride", "power", "weeks", 1); err == nil {
		t.Fatal("bad window kind must be invalid")
	}
}

func TestStatsHeatmap(t *testing.T) {
	repo := inmem.NewActivityRepo()
	user := domain.UserID(uuid.New())
	withTrack := addActivity(repo, user, domain.ActivityTypeRide, vNow.Add(-time.Hour), 1000, domain.PrivacyPrivate)
	sid := domain.SourceID(uuid.New())
	a := repo.Activities[withTrack]
	a.PrimaryStreamSourceID = &sid
	repo.Activities[withTrack] = a
	addActivity(repo, user, domain.ActivityTypeRide, vNow.Add(-2*time.Hour), 1000, domain.PrivacyPrivate) // no stream
	geo := &fakeGeo{tracks: map[domain.SourceID][]domain.GeoPoint{sid: {{Lat: 49, Lon: 8}, {Lat: 49.1, Lon: 8.1}}}}
	hm, err := NewStats(repo, nil, nil, geo, nil).Heatmap(context.Background(), user, port.ActivityListFilter{}, domain.StreamResolution30s)
	if err != nil || hm.Matched != 2 || len(hm.Tracks) != 1 || hm.Tracks[0].Activity.ID != withTrack || hm.Truncated {
		t.Fatalf("heatmap %+v %v", hm, err)
	}
}

func TestHealthSeries(t *testing.T) {
	day := time.Date(2026, 10, 27, 7, 0, 0, 0, time.UTC)
	v := 55.0
	h := NewHealth(&fakeHealthRepo{samples: []health.Sample{{DataType: capability.DataTypeHRV, Timestamp: day, Provider: "garmin", Value: &v, Unit: "ms"}}}, func() time.Time { return vNow })
	series, err := h.Series(context.Background(), domain.UserID(uuid.New()), "HRV", 0)
	if err != nil || len(series) != 1 || series[0].Value != 55 || series[0].Provider != "garmin" {
		t.Fatalf("series %+v %v", series, err)
	}
	if _, err := h.Series(context.Background(), domain.UserID(uuid.New()), "Activity", 10); err == nil {
		t.Fatal("non time-series type must be invalid")
	}
	if _, err := NewHealth(nil, nil).Series(context.Background(), domain.UserID(uuid.New()), "HRV", 10); err == nil {
		t.Fatal("no repo must be unavailable")
	}
}
