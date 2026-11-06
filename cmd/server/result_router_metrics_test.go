package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/types/known/timestamppb"

	workerv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/worker/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/health"
	"github.com/opencairn-org/cairn-core/internal/port"
	metricsuc "github.com/opencairn-org/cairn-core/internal/usecase/metrics"
)

// natKey is the dedup key both stores upsert on.
type natKey struct {
	user, key, provider, extID string
	ts                         int64
}

// fakeMetricRepo upserts on the metrics natural key; err fails every save.
type fakeMetricRepo struct {
	port.MetricRepo
	rows map[natKey]domain.Metric
	err  error
}

func (f *fakeMetricRepo) SaveMetrics(_ context.Context, ms []domain.Metric) error {
	if f.err != nil {
		return f.err
	}
	for _, m := range ms {
		f.rows[natKey{m.UserID.String(), m.Key, m.Provider, m.ExternalID, m.Timestamp.UnixNano()}] = m
	}
	return nil
}

// fakeHealthRepo upserts on the health_samples natural key.
type fakeHealthRepo struct {
	port.HealthRepo
	rows map[natKey]health.Sample
	acct map[natKey]*domain.ExternalAccountID
}

func (f *fakeHealthRepo) SaveSamples(_ context.Context, u domain.UserID, acct *domain.ExternalAccountID, ss []health.Sample) error {
	for _, s := range ss {
		k := natKey{u.String(), string(s.DataType), s.Provider, s.ExternalID, s.Timestamp.UnixNano()}
		f.rows[k], f.acct[k] = s, acct
	}
	return nil
}

func metricEvent(user, acct, extID, key string, ts time.Time, num *float64, js string) *workerv1.WorkerEvent {
	return &workerv1.WorkerEvent{Type: workerv1.WorkerEventType_WORKER_EVENT_TYPE_METRIC,
		Entity: &workerv1.WorkerEvent_Metric{Metric: &workerv1.ImportedMetric{
			Ref: &workerv1.ExternalRef{UserId: user, Provider: "whoop", ExternalAccountId: acct, ExternalId: extID},
			Key: key, Ts: timestamppb.New(ts), ValueNumeric: num, ValueJson: js,
		}}}
}

func num(v float64) *float64 { return &v }

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

// warnings decodes the Warn records of a JSON log buffer.
func warnings(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["level"] == "WARN" {
			out = append(out, rec)
		}
	}
	return out
}

func TestProcessJobResult_MetricRouting(t *testing.T) {
	const subject = "cairn.results.import_metrics.whoop.whoop-pool"
	user, acct := uuid.NewString(), uuid.NewString()
	ts := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)
	stages := `{"stages":[{"start":"2026-11-01T00:00:00Z","end":"2026-11-01T01:00:00Z","stage":"deep"}]}`

	cases := []struct {
		name     string
		ev       *workerv1.WorkerEvent
		store    string // health_series, metrics, or "" when refused
		extID    string // stored external id
		asStruct bool   // stored as value_struct
		reason   string // refusal reason
	}{
		{"data type numeric", metricEvent(user, acct, "cycle:1:Steps", "Steps", ts, num(8000), ""), "health_series", "cycle:1:Steps", false, ""},
		{"data type value_json", metricEvent(user, acct, "sleep:9:Sleep", "Sleep", ts, nil, stages), "health_series", "sleep:9:Sleep", true, ""},
		{"reserved numeric", metricEvent(user, acct, "spo2:1", "spo2_pct", ts, num(97), ""), "metrics", "spo2:1", false, ""},
		{"sleep_stages value_json", metricEvent(user, acct, "sleep:9:stages", "sleep_stages", ts, nil, stages), "metrics", "sleep:9:stages", true, ""},
		{"namespaced, derived external id", metricEvent(user, acct, "", "whoop.recovery_score", ts, num(71), ""), "metrics",
			metricsuc.DerivedExternalID("whoop.recovery_score", ts), false, ""},
		{"invalid key", metricEvent(user, acct, "x:1", "Recovery Score", ts, num(71), ""), "", "", false, metricsuc.RefusedInvalidKey},
		{"shape mismatch", metricEvent(user, acct, "x:2", "spo2_pct", ts, nil, `{"v":97}`), "", "", false, metricsuc.RefusedShapeMismatch},
		{"both values", metricEvent(user, acct, "x:3", "HRV", ts, num(50), `{"v":50}`), "", "", false, metricsuc.RefusedValue},
		{"bad user id", metricEvent("nope", acct, "x:4", "steps", ts, num(1), ""), "", "", false, metricsuc.RefusedBadUserID},
		{"another user's account", metricEvent(uuid.NewString(), acct, "x:5", "Weight", ts, num(70), ""), "", "", false, metricsuc.RefusedAccountMismatch},
		{"unknown account", metricEvent(user, uuid.NewString(), "x:6", "whoop.strain", ts, num(9), ""), "", "", false, metricsuc.RefusedAccountMismatch},
		{"malformed account", metricEvent(user, "nope", "x:7", "Steps", ts, num(1), ""), "", "", false, metricsuc.RefusedBadAccountID},
		{"no account", metricEvent(user, "", "x:8", "spo2_pct", ts, num(97), ""), "", "", false, metricsuc.RefusedBadAccountID},
	}

	mr := &fakeMetricRepo{rows: map[natKey]domain.Metric{}}
	hr := &fakeHealthRepo{rows: map[natKey]health.Sample{}, acct: map[natKey]*domain.ExternalAccountID{}}
	acctID := domain.ExternalAccountID(uuid.MustParse(acct))
	accounts := fakeAccountLookup{acctID: {ID: acctID, UserID: domain.UserID(uuid.MustParse(user)), Provider: "whoop"}}
	app := &App{MetricStore: &metricsuc.Store{Repo: mr, Health: hr, Accounts: accounts}}
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	jr := &workerv1.JobResult{WorkerName: "whoop-pool", WorkerVersion: "1.4"}
	for _, tc := range cases {
		jr.Events = append(jr.Events, tc.ev)
	}

	stored := counterValue(t, workerMetricsStored.WithLabelValues("whoop", "metrics"))
	refusedKey := counterValue(t, workerMetricsRefused.WithLabelValues("whoop", metricsuc.RefusedInvalidKey))
	// The second delivery is a re-import: same rows, not new ones.
	for range 2 {
		if err := processJobResult(context.Background(), app, log, subject, jr); err != nil {
			t.Fatalf("refusals must not fail the result: %v", err)
		}
	}
	if got := counterValue(t, workerMetricsStored.WithLabelValues("whoop", "metrics")) - stored; got != 6 {
		t.Errorf("stored counter +%v; want +6", got)
	}
	if got := counterValue(t, workerMetricsRefused.WithLabelValues("whoop", metricsuc.RefusedInvalidKey)) - refusedKey; got != 2 {
		t.Errorf("refused counter +%v; want +2", got)
	}
	if len(hr.rows) != 2 || len(mr.rows) != 3 {
		t.Fatalf("health rows=%d metrics rows=%d; want 2 and 3", len(hr.rows), len(mr.rows))
	}
	warns := warnings(t, &buf)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			im := tc.ev.GetMetric()
			switch tc.store {
			case "health_series":
				k := natKey{user, im.GetKey(), "whoop", tc.extID, ts.UnixNano()}
				s, ok := hr.rows[k]
				if !ok {
					t.Fatalf("no health sample for %+v", k)
				}
				if tc.asStruct != (s.Value == nil) || tc.asStruct != (len(s.ValueStruct) > 0) || hr.acct[k].String() != acct {
					t.Fatalf("sample = %+v acct=%v", s, hr.acct[k])
				}
			case "metrics":
				k := natKey{user, im.GetKey(), "whoop", tc.extID, ts.UnixNano()}
				m, ok := mr.rows[k]
				if !ok {
					t.Fatalf("no metric for %+v", k)
				}
				if tc.asStruct != (m.ValueNumeric == nil) || tc.asStruct != (len(m.ValueStruct) > 0) || m.ExternalAccountID.String() != acct ||
					m.SourceWorkerName != "whoop-pool" || m.SourceWorkerVersion != "1.4" {
					t.Fatalf("metric = %+v", m)
				}
			default:
				n := 0
				for _, w := range warns {
					if w["msg"] == "worker metric refused" && w["key"] == im.GetKey() && w["reason"] == tc.reason &&
						w["provider"] == "whoop" && w["worker"] == "whoop-pool" {
						n++
					}
				}
				if n != 2 {
					t.Fatalf("refusal warnings for %s/%s = %d; want 2 (one per delivery)\n%v", im.GetKey(), tc.reason, n, warns)
				}
			}
		})
	}
}

func TestIngestMetricEvent_StorageErrors(t *testing.T) {
	user, acct := uuid.New(), domain.ExternalAccountID(uuid.New())
	accounts := fakeAccountLookup{acct: {ID: acct, UserID: domain.UserID(user), Provider: "whoop"}}
	ev := metricEvent(user.String(), acct.String(), "spo2:1", "spo2_pct", time.Now(), num(97), "")
	for _, tc := range []struct {
		name string
		err  error
		nak  bool
	}{
		{"transient NAKs", errors.New("connection reset"), true},
		{"deterministic is refused", fmt.Errorf("save metric 0: %w", &pgconn.PgError{Code: "23503"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{MetricStore: &metricsuc.Store{Repo: &fakeMetricRepo{err: tc.err}, Accounts: accounts}}
			var buf bytes.Buffer
			err := processWorkerEvent(context.Background(), app, slog.New(slog.NewJSONHandler(&buf, nil)),
				&workerv1.JobResult{}, &ingestBundle{provider: "whoop"}, ev)
			if (err != nil) != tc.nak {
				t.Fatalf("err = %v; want NAK %v", err, tc.nak)
			}
			if w := warnings(t, &buf); !tc.nak && (len(w) != 1 || w[0]["reason"] != "rejected_by_db") {
				t.Fatalf("warnings = %v", w)
			}
		})
	}
}

func TestIngestMetricEvent_NoStoreIsLogged(t *testing.T) {
	var buf bytes.Buffer
	ev := &workerv1.WorkerEvent{Type: workerv1.WorkerEventType_WORKER_EVENT_TYPE_METRIC}
	if err := processWorkerEvent(context.Background(), &App{}, slog.New(slog.NewJSONHandler(&buf, nil)),
		&workerv1.JobResult{}, &ingestBundle{}, ev); err != nil {
		t.Fatal(err)
	}
	if w := warnings(t, &buf); len(w) != 1 || w[0]["reason"] != "missing_metric" {
		t.Fatalf("warnings = %v", w)
	}
}
