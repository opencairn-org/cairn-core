package rest

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/besteffort"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// RecordHandlers serve personal records and best-effort progressions.
//
//	GET /api/records                  all-time best per (type, metric, window)
//	GET /api/best-efforts/history?type=&metric=&window_kind=&window_value=
//	  → { type, metric, window_kind, window_value, best, count, items:[...] }
type RecordHandlers struct {
	Auth  *Auth
	Stats *view.Stats
	Log   *slog.Logger
}

// Mount registers the routes.
func (h *RecordHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/records", h.records)
	mux.HandleFunc("GET /api/best-efforts/history", h.history)
}

func (h *RecordHandlers) records(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	recs, err := h.Stats.PersonalRecords(r.Context(), userID)
	if err != nil {
		h.Log.Error("list personal records failed", "user_id", userID, "error", err)
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(recs))
	for _, p := range recs {
		out = append(out, map[string]any{
			"activity_type":  string(p.ActivityType),
			"metric":         string(p.Metric),
			"window_kind":    string(p.WindowKind),
			"window_value":   p.WindowValue,
			"achieved_value": p.AchievedValue,
			"activity_id":    p.ActivityID.String(),
			"timestamp":      p.Timestamp.UTC(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": out})
}

func (h *RecordHandlers) history(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	activityType, metric, windowKind := q.Get("type"), q.Get("metric"), q.Get("window_kind")
	windowValue, _ := strconv.Atoi(q.Get("window_value"))
	hist, err := h.Stats.BestEffortHistory(r.Context(), userID, activityType, metric, windowKind, windowValue)
	if err != nil {
		h.Log.Error("best-effort history failed", "user", userID, "error", err)
		writeActionError(w, err, "load failed")
		return
	}
	items := make([]map[string]any, 0, len(hist.Items))
	for _, it := range hist.Items {
		items = append(items, map[string]any{
			"activity_id":    it.ActivityID.String(),
			"title":          it.Title,
			"start_time":     it.StartTime.UTC(),
			"achieved_value": it.AchievedValue,
			"is_best":        it.AchievedValue == hist.Best,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type":         activityType,
		"metric":       metric,
		"window_kind":  windowKind,
		"window_value": windowValue,
		"best":         hist.Best,
		"count":        len(hist.Items),
		"items":        items,
	})
}

// CustomRecordHandlers are the REST twin of the custom best-effort RPCs.
//
//	GET    /api/records/custom        → { custom_best_efforts: [...] }
//	POST   /api/records/custom        { activity_type, metric, window_kind, window_value, label }
//	DELETE /api/records/custom/{id}
type CustomRecordHandlers struct {
	Auth    *Auth
	Customs *besteffort.Customs
	// Backfill recomputes the user's efforts of one type; it must not block.
	Backfill func(userID domain.UserID, at domain.ActivityType)
}

// Mount registers the routes.
func (h *CustomRecordHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/records/custom", h.list)
	mux.HandleFunc("POST /api/records/custom", h.create)
	mux.HandleFunc("DELETE /api/records/custom/{id}", h.del)
}

func (h *CustomRecordHandlers) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	list, err := h.Customs.List(r.Context(), userID)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		out = append(out, dashCustomBestEffortJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"custom_best_efforts": out})
}

func (h *CustomRecordHandlers) create(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		ActivityType string `json:"activity_type"`
		Metric       string `json:"metric"`
		WindowKind   string `json:"window_kind"`
		WindowValue  int    `json:"window_value"`
		Label        string `json:"label"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	created, err := h.Customs.Create(r.Context(), domain.CustomBestEffort{
		UserID: userID, ActivityType: domain.ActivityType(body.ActivityType),
		Metric: domain.BestEffortMetric(body.Metric), WindowKind: domain.BestEffortWindowKind(body.WindowKind),
		WindowValue: body.WindowValue, Label: body.Label,
	})
	if err != nil {
		writeActionError(w, err, "create failed")
		return
	}
	if h.Backfill != nil {
		h.Backfill(userID, created.ActivityType)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"custom_best_effort": dashCustomBestEffortJSON(created)})
}

func (h *CustomRecordHandlers) del(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.CustomBestEffortID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.Customs.Delete(r.Context(), userID, id); err != nil {
		writeActionError(w, err, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func dashCustomBestEffortJSON(c domain.CustomBestEffort) map[string]any {
	return map[string]any{
		"id": c.ID.String(), "activity_type": string(c.ActivityType), "metric": string(c.Metric),
		"window_kind": string(c.WindowKind), "window_value": c.WindowValue, "label": c.Label,
		"created_at": c.CreatedAt.UTC().Format(time.RFC3339),
	}
}
