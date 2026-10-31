package rest

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/instance"
	metricsuc "github.com/opencairn-org/cairn-core/internal/usecase/metrics"
)

// MetricHandlers are the REST twins of the MetricsService store RPCs.
//
//	GET    /api/metrics?keys=a,b&from=&to=&providers=&external_account_id=&activity_id=&sort=asc|desc&limit=&offset=
//	GET    /api/metrics/latest?keys=a,b&as_of=
//	GET    /api/metrics/series?key=&from=&to=&bucket=hour|day|week|month&aggregation=avg|min|max|sum|last|first|count&max_points=&providers=
//	GET    /api/metrics/context?as_of=
//	PUT    /api/metrics            { id?, key, ts, period_seconds?, value_numeric?|value_struct?, provider?, tags?, notes?, dedup_by_natural_key? }
//	POST   /api/metrics/bulk       { metrics: [...], continue_on_error? }
//	DELETE /api/metrics/{id}
type MetricHandlers struct {
	Auth  *Auth
	Store *metricsuc.Store
	// Policies adds the merge policies to /context (nil → omitted).
	Policies *instance.MergePolicies
}

// Mount registers the routes.
func (h *MetricHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/metrics", h.list)
	mux.HandleFunc("GET /api/metrics/latest", h.latest)
	mux.HandleFunc("GET /api/metrics/series", h.series)
	mux.HandleFunc("GET /api/metrics/context", h.context)
	mux.HandleFunc("PUT /api/metrics", h.upsert)
	mux.HandleFunc("POST /api/metrics/bulk", h.bulk)
	mux.HandleFunc("DELETE /api/metrics/{id}", h.remove)
}

// actCSV splits a comma-separated query value, dropping blanks.
func actCSV(raw string) []string {
	var out []string
	for p := range strings.SplitSeq(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// actQueryTime parses an RFC 3339 query value; ok is false (400 written) on a malformed one.
func actQueryTime(w http.ResponseWriter, r *http.Request, name string) (time.Time, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		http.Error(w, name+" must be RFC 3339", http.StatusBadRequest)
		return time.Time{}, false
	}
	return t, true
}

func actQueryInt(r *http.Request, name string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(name))
	return n
}

func actOptionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (h *MetricHandlers) list(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := domain.MetricListFilter{Keys: actCSV(q.Get("keys")), Providers: actCSV(q.Get("providers")), Ascending: q.Get("sort") == "asc",
		Limit: actQueryInt(r, "limit"), Offset: actQueryInt(r, "offset")}
	if f.Start, ok = actQueryTime(w, r, "from"); !ok {
		return
	}
	if f.End, ok = actQueryTime(w, r, "to"); !ok {
		return
	}
	if raw := q.Get("external_account_id"); raw != "" {
		acct, err := domain.ParseUUID[domain.ExternalAccountID](raw)
		if err != nil {
			http.Error(w, "bad external_account_id", http.StatusBadRequest)
			return
		}
		f.ExternalAccountID = &acct
	}
	if raw := q.Get("activity_id"); raw != "" {
		act, err := domain.ParseUUID[domain.ActivityID](raw)
		if err != nil {
			http.Error(w, "bad activity_id", http.StatusBadRequest)
			return
		}
		f.ActivityID = &act
	}
	rows, total, err := h.Store.List(r.Context(), me, f)
	if err != nil {
		writeActionError(w, err, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		out = append(out, actMetricJSON(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"metrics": out, "total": total})
}

func (h *MetricHandlers) latest(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	asOf, ok := actQueryTime(w, r, "as_of")
	if !ok {
		return
	}
	latest, err := h.Store.Latest(r.Context(), me, actCSV(r.URL.Query().Get("keys")), actOptionalTime(asOf))
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"metrics": actMetricsMapJSON(latest)})
}

func (h *MetricHandlers) series(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	sq := domain.MetricSeriesQuery{Key: q.Get("key"), Bucket: domain.MetricBucket(q.Get("bucket")), Aggregation: domain.MetricAggregation(q.Get("aggregation")),
		Providers: actCSV(q.Get("providers")), MaxPoints: actQueryInt(r, "max_points")}
	if sq.Start, ok = actQueryTime(w, r, "from"); !ok {
		return
	}
	if sq.End, ok = actQueryTime(w, r, "to"); !ok {
		return
	}
	points, err := h.Store.Series(r.Context(), me, sq)
	if err != nil {
		writeActionError(w, err, "query failed")
		return
	}
	out := make([]map[string]any, 0, len(points))
	for _, p := range points {
		out = append(out, map[string]any{"ts": p.Ts.UTC().Format(time.RFC3339), "value": p.Value, "sample_count": p.Samples})
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": sq.Key, "bucket": string(sq.Bucket), "aggregation": string(sq.Aggregation), "points": out})
}

func (h *MetricHandlers) context(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	asOf, ok := actQueryTime(w, r, "as_of")
	if !ok {
		return
	}
	latest, err := h.Store.Context(r.Context(), me, actOptionalTime(asOf))
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := map[string]any{"latest_metrics": actMetricsMapJSON(latest), "settings": map[string]any{"user_id": me.String()}}
	if h.Policies != nil {
		policies, err := h.Policies.ForUser(r.Context(), me)
		if err != nil {
			http.Error(w, "load failed", http.StatusInternalServerError)
			return
		}
		out["settings"] = map[string]any{"user_id": me.String(), "merge_policy_by_activity_type": actPoliciesJSON(policies)}
	}
	writeJSON(w, http.StatusOK, out)
}

// actMetricBody is the JSON shape of one manual entry.
type actMetricBody struct {
	ID                string          `json:"id"`
	Key               string          `json:"key"`
	Ts                time.Time       `json:"ts"`
	PeriodSeconds     int             `json:"period_seconds"`
	ValueNumeric      *float64        `json:"value_numeric"`
	ValueStruct       json.RawMessage `json:"value_struct"`
	Provider          string          `json:"provider"`
	Tags              []string        `json:"tags"`
	Notes             string          `json:"notes"`
	DedupByNaturalKey bool            `json:"dedup_by_natural_key"`
}

func (b actMetricBody) input() (metricsuc.UpsertInput, error) {
	in := metricsuc.UpsertInput{Key: b.Key, Ts: b.Ts, PeriodSeconds: b.PeriodSeconds, ValueNumeric: b.ValueNumeric, Provider: b.Provider,
		Tags: b.Tags, Notes: b.Notes, DedupByNaturalKey: b.DedupByNaturalKey}
	if string(b.ValueStruct) != "" && string(b.ValueStruct) != "null" {
		in.ValueStruct = b.ValueStruct
	}
	if b.ID != "" {
		id, err := domain.ParseUUID[domain.MetricID](b.ID)
		if err != nil {
			return in, err
		}
		in.ID = &id
	}
	return in, nil
}

func (h *MetricHandlers) upsert(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	var body actMetricBody
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	in, err := body.input()
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	m, created, err := h.Store.Upsert(r.Context(), me, in)
	if err != nil {
		writeActionError(w, err, "save failed")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"metric": actMetricJSON(m), "created": created})
}

func (h *MetricHandlers) bulk(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Metrics         []actMetricBody `json:"metrics"`
		ContinueOnError bool            `json:"continue_on_error"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	rows := make([]metricsuc.UpsertInput, 0, len(body.Metrics))
	for i, b := range body.Metrics {
		in, err := b.input()
		if err != nil {
			http.Error(w, "row "+strconv.Itoa(i)+": bad id", http.StatusBadRequest)
			return
		}
		rows = append(rows, in)
	}
	res, err := h.Store.BulkUpsert(r.Context(), me, rows, body.ContinueOnError)
	if err != nil {
		writeActionError(w, err, "save failed")
		return
	}
	errs := make([]map[string]any, 0, len(res.Errors))
	for _, e := range res.Errors {
		errs = append(errs, map[string]any{"row_index": e.Row, "code": e.Code, "message": e.Message})
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": res.Created, "updated": res.Updated, "skipped": res.Skipped, "errors": errs})
}

func (h *MetricHandlers) remove(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.MetricID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.Store.Delete(r.Context(), me, id); err != nil {
		writeActionError(w, err, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func actMetricsMapJSON(m map[string]domain.Metric) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = actMetricJSON(v)
	}
	return out
}

func actMetricJSON(m domain.Metric) map[string]any {
	out := map[string]any{
		"id": m.ID.String(), "user_id": m.UserID.String(), "key": m.Key, "ts": m.Timestamp.UTC().Format(time.RFC3339),
		"period_seconds": m.PeriodSeconds, "value_numeric": m.ValueNumeric, "provider": m.Provider, "tags": m.Tags, "notes": m.Notes,
		"external_account_id": nil, "external_id": nil, "activity_id": nil, "value_struct": nil,
		"source_worker_name": m.SourceWorkerName, "source_worker_version": m.SourceWorkerVersion,
	}
	if m.Tags == nil {
		out["tags"] = []string{}
	}
	if len(m.ValueStruct) > 0 {
		out["value_struct"] = m.ValueStruct
	}
	if m.ExternalAccountID != nil {
		out["external_account_id"] = m.ExternalAccountID.String()
	}
	if m.ExternalID != "" {
		out["external_id"] = m.ExternalID
	}
	if m.ActivityID != nil {
		out["activity_id"] = m.ActivityID.String()
	}
	if !m.CreatedAt.IsZero() {
		out["created_at"] = m.CreatedAt.UTC().Format(time.RFC3339)
		out["updated_at"] = m.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return out
}
