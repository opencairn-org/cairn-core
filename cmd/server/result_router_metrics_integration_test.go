//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	workerv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/worker/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	metricsuc "github.com/opencairn-org/cairn-core/internal/usecase/metrics"
)

// TestResultRouter_WorkerMetrics routes one import_metrics result twice into Postgres; the redelivery adds no row.
func TestResultRouter_WorkerMetrics(t *testing.T) {
	env := newITestEnv(t)
	alice := env.user("alice", domain.UserRoleUser)
	ctx := context.Background()
	acct, err := env.app.ExternalAccounts.CreateAccount(ctx, domain.ExternalAccount{
		UserID: alice.userID, Provider: "garmin", ProviderAccountID: "g-1", Status: domain.ExternalAccountStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Truncate(time.Second).Add(-24 * time.Hour)
	stages := `{"stages": [{"end": "2026-11-01T01:00:00Z", "stage": "deep", "start": "2026-11-01T00:00:00Z"}]}`
	ev := func(extID, key string, v *float64, js string) *workerv1.WorkerEvent {
		return &workerv1.WorkerEvent{Type: workerv1.WorkerEventType_WORKER_EVENT_TYPE_METRIC,
			Entity: &workerv1.WorkerEvent_Metric{Metric: &workerv1.ImportedMetric{
				Ref: &workerv1.ExternalRef{UserId: alice.userID.String(), Provider: "garmin", ExternalAccountId: acct.String(), ExternalId: extID},
				Key: key, Ts: timestamppb.New(ts), ValueNumeric: v, ValueJson: js,
			}}}
	}
	jr := &workerv1.JobResult{WorkerName: "garmin-pool", WorkerVersion: "2.3", Events: []*workerv1.WorkerEvent{
		ev("day:1:Steps", "Steps", num(8000), ""),
		ev("sleep:1:Sleep", "Sleep", nil, stages),
		ev("spo2:1", "spo2_pct", num(97), ""),
		ev("sleep:1:stages", "sleep_stages", nil, stages),
		ev("", "garmin.body_battery", num(64), ""),
		ev("x:1", "Body Battery", num(64), ""),
		ev("x:2", "spo2_pct", nil, `{"v": 97}`),
	}}
	for range 2 {
		if err := processJobResult(ctx, env.app, testLogger(), "cairn.results.import_metrics.garmin.garmin-pool", jr); err != nil {
			t.Fatalf("process: %v", err)
		}
	}

	// metrics store, through GET /api/metrics.
	var list struct {
		Metrics []map[string]any `json:"metrics"`
		Total   int              `json:"total"`
	}
	_ = json.Unmarshal(alice.do(http.MethodGet, "/api/metrics", nil, http.StatusOK), &list)
	if list.Total != 3 {
		t.Fatalf("metrics rows = %d; want 3: %+v", list.Total, list.Metrics)
	}
	want := map[string]string{"spo2_pct": "spo2:1", "sleep_stages": "sleep:1:stages",
		"garmin.body_battery": metricsuc.DerivedExternalID("garmin.body_battery", ts)}
	for _, m := range list.Metrics {
		key := str(m, "key")
		if str(m, "external_id") != want[key] || str(m, "provider") != "garmin" || str(m, "external_account_id") != acct.String() ||
			str(m, "source_worker_name") != "garmin-pool" || str(m, "source_worker_version") != "2.3" {
			t.Errorf("metric %s = %v", key, m)
		}
		if key == "sleep_stages" {
			if b, _ := json.Marshal(m["value_struct"]); m["value_numeric"] != nil || !jsonEqual(t, b, []byte(stages)) {
				t.Errorf("sleep_stages value = %v / %s", m["value_numeric"], b)
			}
		}
	}

	// health_samples: raw rows keep the ref's external id and the struct value.
	for dt, extID := range map[capability.DataType]string{capability.DataTypeSteps: "day:1:Steps", capability.DataTypeSleep: "sleep:1:Sleep"} {
		ss, err := env.app.Health.ListSamples(ctx, alice.userID, dt, ts.Add(-time.Hour), ts.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(ss) != 1 || ss[0].ExternalID != extID || ss[0].Provider != "garmin" {
			t.Fatalf("%s samples = %+v", dt, ss)
		}
		if dt == capability.DataTypeSleep && (ss[0].Value != nil || !jsonEqual(t, ss[0].ValueStruct, []byte(stages))) {
			t.Fatalf("sleep sample = %v / %s", ss[0].Value, ss[0].ValueStruct)
		}
	}
	var series struct {
		Series []map[string]any `json:"series"`
	}
	_ = json.Unmarshal(alice.do(http.MethodGet, "/api/health?type=Steps&days=7", nil, http.StatusOK), &series)
	if len(series.Series) != 1 || series.Series[0]["value"] != 8000.0 {
		t.Fatalf("steps series = %+v", series)
	}
}

// jsonEqual compares two JSON documents semantically.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("json %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("json %s: %v", b, err)
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}
