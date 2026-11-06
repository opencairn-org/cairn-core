package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/domain/health"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeHealth struct {
	port.HealthRepo
	samples []health.Sample
	acct    *domain.ExternalAccountID
}

func (f *fakeHealth) SaveSamples(_ context.Context, _ domain.UserID, acct *domain.ExternalAccountID, ss []health.Sample) error {
	f.samples, f.acct = append(f.samples, ss...), acct
	return nil
}

// fakeAccounts serves accounts by id; err fails every lookup.
type fakeAccounts struct {
	rows map[domain.ExternalAccountID]domain.ExternalAccount
	err  error
}

func (f fakeAccounts) GetExternalAccount(_ context.Context, id domain.ExternalAccountID) (domain.ExternalAccount, error) {
	if f.err != nil {
		return domain.ExternalAccount{}, f.err
	}
	a, ok := f.rows[id]
	if !ok {
		return domain.ExternalAccount{}, domain.ErrNotFound
	}
	return a, nil
}

func TestRouteImport(t *testing.T) {
	for key, want := range map[string]ImportTarget{
		"Steps": TargetHealthSeries, "HRV": TargetHealthSeries, "Sleep": TargetHealthSeries, "WaterIntake": TargetHealthSeries,
		"Activity": TargetMetrics, "Gear": TargetMetrics, "steps": TargetMetrics, "spo2_pct": TargetMetrics, "whoop.recovery_score": TargetMetrics,
	} {
		if got := RouteImport(key); got != want {
			t.Errorf("RouteImport(%q) = %s; want %s", key, got, want)
		}
	}
}

func TestStoreImport(t *testing.T) {
	ctx := context.Background()
	user, acct, stranger := domain.UserID(uuid.New()), domain.ExternalAccountID(uuid.New()), domain.ExternalAccountID(uuid.New())
	accounts := fakeAccounts{rows: map[domain.ExternalAccountID]domain.ExternalAccount{acct: {ID: acct, UserID: user, Provider: "whoop"}}}
	ts := time.Date(2026, 11, 1, 6, 30, 0, 0, time.FixedZone("CET", 3600))
	base := ImportInput{UserID: user, Ts: ts, Provider: "whoop", ExternalAccountID: &acct, ExternalID: "cycle:1",
		SourceWorkerName: "whoop", SourceWorkerVersion: "1.2"}
	with := func(f func(*ImportInput)) ImportInput { in := base; f(&in); return in }
	stages := json.RawMessage(`{"stages":[{"start":"2026-11-01T00:00:00Z","end":"2026-11-01T01:00:00Z","stage":"deep"}]}`)

	cases := []struct {
		name   string
		in     ImportInput
		target ImportTarget
		reason string // empty: stored
	}{
		{"data type numeric", with(func(in *ImportInput) { in.Key, in.ValueNumeric = "Steps", f64(8000) }), TargetHealthSeries, ""},
		{"data type struct", with(func(in *ImportInput) { in.Key, in.ValueStruct = "Sleep", stages }), TargetHealthSeries, ""},
		{"reserved numeric", with(func(in *ImportInput) { in.Key, in.ValueNumeric = "spo2_pct", f64(97) }), TargetMetrics, ""},
		{"reserved struct", with(func(in *ImportInput) { in.Key, in.ValueStruct = "sleep_stages", stages }), TargetMetrics, ""},
		{"namespaced, no external id", with(func(in *ImportInput) { in.Key, in.ValueNumeric, in.ExternalID = "whoop.recovery_score", f64(71), "" }), TargetMetrics, ""},
		{"invalid key", with(func(in *ImportInput) { in.Key, in.ValueNumeric = "Recovery Score", f64(1) }), TargetMetrics, RefusedInvalidKey},
		{"shape mismatch", with(func(in *ImportInput) { in.Key, in.ValueStruct = "spo2_pct", json.RawMessage(`{"v":97}`) }), TargetMetrics, RefusedShapeMismatch},
		{"struct key numeric", with(func(in *ImportInput) { in.Key, in.ValueNumeric = "sleep_stages", f64(1) }), TargetMetrics, RefusedShapeMismatch},
		{"both values", with(func(in *ImportInput) { in.Key, in.ValueNumeric, in.ValueStruct = "HRV", f64(50), stages }), TargetHealthSeries, RefusedValue},
		{"no value", with(func(in *ImportInput) { in.Key = "spo2_pct" }), TargetMetrics, RefusedValue},
		{"json null is no value", with(func(in *ImportInput) { in.Key, in.ValueStruct = "Weight", json.RawMessage(" null ") }), TargetHealthSeries, RefusedValue},
		{"bad json", with(func(in *ImportInput) { in.Key, in.ValueStruct = "sleep_stages", json.RawMessage(`{"stages":`) }), TargetMetrics, RefusedBadJSON},
		{"no timestamp", with(func(in *ImportInput) { in.Key, in.ValueNumeric, in.Ts = "Steps", f64(1), time.Time{} }), TargetHealthSeries, RefusedMissingTimestamp},
		{"no timestamp metric", with(func(in *ImportInput) { in.Key, in.ValueNumeric, in.Ts = "steps", f64(1), time.Time{} }), TargetMetrics, RefusedMissingTimestamp},
		{"no user", with(func(in *ImportInput) { in.Key, in.ValueNumeric, in.UserID = "steps", f64(1), domain.UserID{} }), TargetMetrics, RefusedBadUserID},
		{"no account", with(func(in *ImportInput) { in.Key, in.ValueNumeric, in.ExternalAccountID = "Steps", f64(1), nil }), TargetHealthSeries, RefusedBadAccountID},
		{"unknown account", with(func(in *ImportInput) { in.Key, in.ValueNumeric, in.ExternalAccountID = "spo2_pct", f64(97), &stranger }), TargetMetrics, RefusedAccountMismatch},
		{"account of another user", with(func(in *ImportInput) {
			in.Key, in.ValueNumeric, in.UserID = "Steps", f64(1), domain.UserID(uuid.New())
		}), TargetHealthSeries, RefusedAccountMismatch},
		{"account of another provider", with(func(in *ImportInput) { in.Key, in.ValueNumeric, in.Provider = "spo2_pct", f64(97), "oura" }), TargetMetrics, RefusedAccountMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, hr := &fakeRepo{rows: map[domain.MetricID]domain.Metric{}}, &fakeHealth{}
			s := &Store{Repo: repo, Health: hr, Accounts: accounts}
			err := s.Import(ctx, tc.in)
			if got := RouteImport(tc.in.Key); got != tc.target {
				t.Fatalf("target = %s; want %s", got, tc.target)
			}
			if tc.reason != "" {
				var rf *ImportRefusedError
				if !errors.As(err, &rf) || rf.Reason != tc.reason {
					t.Fatalf("err = %v; want refusal %s", err, tc.reason)
				}
				if len(repo.rows)+len(hr.samples) != 0 {
					t.Fatalf("refused metric stored: %d metrics, %d samples", len(repo.rows), len(hr.samples))
				}
				return
			}
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			if tc.target == TargetHealthSeries {
				if len(hr.samples) != 1 || len(repo.rows) != 0 {
					t.Fatalf("samples=%d metrics=%d", len(hr.samples), len(repo.rows))
				}
				got := hr.samples[0]
				if got.DataType != capability.DataType(tc.in.Key) || got.ExternalID != "cycle:1" || got.Provider != "whoop" ||
					!got.Timestamp.Equal(ts) || got.Timestamp.Location() != time.UTC || *hr.acct != acct {
					t.Fatalf("sample = %+v acct=%v", got, hr.acct)
				}
				if (got.Value == nil) == (got.ValueStruct == nil) {
					t.Fatalf("sample value = %v / %s", got.Value, got.ValueStruct)
				}
				return
			}
			if len(repo.rows) != 1 || len(hr.samples) != 0 {
				t.Fatalf("metrics=%d samples=%d", len(repo.rows), len(hr.samples))
			}
			for _, m := range repo.rows {
				wantExt := tc.in.ExternalID
				if wantExt == "" {
					wantExt = "derived:whoop.recovery_score@2026-11-01T05:30:00Z"
				}
				if m.Key != tc.in.Key || m.ExternalID != wantExt || m.Provider != "whoop" || *m.ExternalAccountID != acct ||
					m.SourceWorkerName != "whoop" || m.SourceWorkerVersion != "1.2" || !m.Timestamp.Equal(ts) || m.ID == (domain.MetricID{}) {
					t.Fatalf("metric = %+v", m)
				}
			}
		})
	}
}

func TestStoreImportUnavailable(t *testing.T) {
	in := ImportInput{UserID: domain.UserID(uuid.New()), Key: "Steps", Ts: time.Now(), ValueNumeric: f64(1)}
	for _, s := range []*Store{nil, {}} {
		for _, key := range []string{"Steps", "spo2_pct"} {
			in.Key = key
			var rf *ImportRefusedError
			if err := s.Import(context.Background(), in); !errors.As(err, &rf) || rf.Reason != RefusedUnavailable {
				t.Fatalf("%s: err = %v; want %s", key, err, RefusedUnavailable)
			}
		}
	}
}

func TestStoreImportAccountLookupError(t *testing.T) {
	acct := domain.ExternalAccountID(uuid.New())
	s := &Store{Repo: &fakeRepo{rows: map[domain.MetricID]domain.Metric{}}, Accounts: fakeAccounts{err: errors.New("db down")}}
	err := s.Import(context.Background(), ImportInput{UserID: domain.UserID(uuid.New()), Key: "spo2_pct", Ts: time.Now(),
		ValueNumeric: f64(97), Provider: "whoop", ExternalAccountID: &acct})
	var rf *ImportRefusedError
	if err == nil || errors.As(err, &rf) {
		t.Fatalf("lookup failure must be retryable, got %v", err)
	}
}
