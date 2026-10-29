//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// TestAPI_Metrics walks the metric store on REST and the MetricsService RPCs:
// manual entry, natural-key dedup, bulk import, latest, series, context, delete.
func TestAPI_Metrics(t *testing.T) {
	env := newITestEnv(t)
	alice := env.user("alice", domain.UserRoleUser)
	bob := env.user("bob", domain.UserRoleUser)
	day := time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC)

	// REST: create, dedup-edit, bulk with one bad row, list and latest.
	created := alice.put("/api/metrics", map[string]any{"key": "weight_kg", "ts": day, "value_numeric": 80.0, "tags": []string{"morning"}}, http.StatusCreated)
	metric, _ := created["metric"].(map[string]any)
	id := str(metric, "id")
	if id == "" || str(metric, "provider") != "manual" || created["created"] != true {
		t.Fatalf("create: %v", created)
	}
	edited := alice.put("/api/metrics", map[string]any{"key": "weight_kg", "ts": day, "value_numeric": 79.5, "dedup_by_natural_key": true}, http.StatusOK)
	if m, _ := edited["metric"].(map[string]any); str(m, "id") != id || m["value_numeric"] != 79.5 {
		t.Fatalf("dedup edit: %v", edited)
	}
	alice.do(http.MethodPut, "/api/metrics", map[string]any{"key": "weight_kg", "ts": day}, http.StatusBadRequest)
	bulk := alice.post("/api/metrics/bulk", map[string]any{"continue_on_error": true, "metrics": []map[string]any{
		{"key": "weight_kg", "ts": day.Add(24 * time.Hour), "value_numeric": 79.0},
		{"key": "Nope", "ts": day, "value_numeric": 1},
		{"key": "resting_hr_bpm", "ts": day.Add(48 * time.Hour), "value_numeric": 47.0},
	}}, http.StatusOK)
	if bulk["created"] != 2.0 || len(bulk["errors"].([]any)) != 1 {
		t.Fatalf("bulk: %v", bulk)
	}
	var list struct {
		Metrics []map[string]any `json:"metrics"`
		Total   int              `json:"total"`
	}
	_ = json.Unmarshal(alice.do(http.MethodGet, "/api/metrics?keys=weight_kg&sort=asc", nil, http.StatusOK), &list)
	if list.Total != 2 || len(list.Metrics) != 2 || list.Metrics[0]["value_numeric"] != 79.5 {
		t.Fatalf("list: %+v", list)
	}
	var latest struct {
		Metrics map[string]map[string]any `json:"metrics"`
	}
	_ = json.Unmarshal(alice.do(http.MethodGet, "/api/metrics/latest?keys=weight_kg,resting_hr_bpm", nil, http.StatusOK), &latest)
	if latest.Metrics["weight_kg"]["value_numeric"] != 79.0 || latest.Metrics["resting_hr_bpm"]["value_numeric"] != 47.0 {
		t.Fatalf("latest: %+v", latest)
	}
	var series struct {
		Points []map[string]any `json:"points"`
	}
	_ = json.Unmarshal(alice.do(http.MethodGet, "/api/metrics/series?key=weight_kg&bucket=day&aggregation=avg", nil, http.StatusOK), &series)
	if len(series.Points) != 2 || series.Points[0]["value"] != 79.5 {
		t.Fatalf("series: %+v", series)
	}
	alice.do(http.MethodGet, "/api/metrics/series?key=weight_kg&bucket=fortnight", nil, http.StatusBadRequest)
	var ctxOut struct {
		Latest map[string]map[string]any `json:"latest_metrics"`
	}
	_ = json.Unmarshal(alice.do(http.MethodGet, "/api/metrics/context", nil, http.StatusOK), &ctxOut)
	if ctxOut.Latest["weight_kg"]["value_numeric"] != 79.0 {
		t.Fatalf("context: %+v", ctxOut)
	}
	// Bob neither sees nor deletes Alice's rows.
	_ = json.Unmarshal(bob.do(http.MethodGet, "/api/metrics", nil, http.StatusOK), &list)
	if list.Total != 0 {
		t.Fatalf("bob sees alice: %+v", list)
	}
	bob.do(http.MethodDelete, "/api/metrics/"+id, nil, http.StatusNotFound)

	// RPC twins over the same rows.
	up := alice.post("/cairn.v1.MetricsService/UpsertMetric", map[string]any{"id": id, "key": "weight_kg", "ts": day.Format(time.RFC3339), "valueNumeric": 78.0}, http.StatusOK)
	if m, _ := up["metric"].(map[string]any); str(m, "id") != id || m["valueNumeric"] != 78.0 {
		t.Fatalf("rpc upsert: %v", up)
	}
	alice.rpc("/cairn.v1.MetricsService/UpsertMetric", map[string]any{"id": id, "key": "steps", "ts": day.Format(time.RFC3339), "valueNumeric": 1}, http.StatusBadRequest)
	got := alice.post("/cairn.v1.MetricsService/GetLatestMetrics", map[string]any{"keys": []string{"weight_kg"}, "asOf": day.Add(time.Hour).Format(time.RFC3339)}, http.StatusOK)
	if ms, _ := got["metrics"].(map[string]any); ms["weight_kg"].(map[string]any)["valueNumeric"] != 78.0 {
		t.Fatalf("rpc latest as-of: %v", got)
	}
	qs := alice.post("/cairn.v1.MetricsService/QueryMetricSeries", map[string]any{"key": "weight_kg", "bucket": "METRIC_BUCKET_WEEK", "aggregation": "METRIC_AGGREGATION_MAX"}, http.StatusOK)
	if pts, _ := qs["points"].([]any); len(pts) != 1 || pts[0].(map[string]any)["value"] != 79.0 || str(qs, "aggregation") != "METRIC_AGGREGATION_MAX" {
		t.Fatalf("rpc series: %v", qs)
	}
	bulkRPC := alice.post("/cairn.v1.MetricsService/BulkUpsertMetrics", map[string]any{"metrics": []map[string]any{
		{"key": "hrv_rmssd_ms", "ts": day.Format(time.RFC3339), "valueNumeric": 61},
		{"key": "hrv_rmssd_ms", "ts": day.Format(time.RFC3339), "valueNumeric": 62, "dedupByNaturalKey": true},
	}}, http.StatusOK)
	if bulkRPC["created"] != 1.0 || bulkRPC["updated"] != 1.0 {
		t.Fatalf("rpc bulk: %v", bulkRPC)
	}
	uc := alice.post("/cairn.v1.MetricsService/GetUserContext", map[string]any{}, http.StatusOK)
	if lm, _ := uc["latestMetrics"].(map[string]any); lm["hrv_rmssd_ms"] == nil || uc["settings"].(map[string]any)["userId"] != alice.userID.String() {
		t.Fatalf("rpc context: %v", uc)
	}
	alice.rpc("/cairn.v1.MetricsService/DeleteMetric", map[string]any{"id": id}, http.StatusOK)
	alice.rpc("/cairn.v1.MetricsService/DeleteMetric", map[string]any{"id": id}, http.StatusNotFound)
	alice.do(http.MethodDelete, "/api/metrics/"+id, nil, http.StatusNotFound)
	_ = json.Unmarshal(alice.do(http.MethodGet, "/api/metrics", nil, http.StatusOK), &list)
	if list.Total != 3 {
		t.Fatalf("after delete: %+v", list)
	}
}
