package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// mountCustomRecords is the REST twin of the custom best-effort RPCs:
//
//	GET    /api/records/custom        → { custom_best_efforts: [...] }
//	POST   /api/records/custom        { activity_type, metric, window_kind, window_value, label }
//	DELETE /api/records/custom/{id}
func mountCustomRecords(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.CustomBestEfforts == nil {
		return
	}
	h := &customRecordHandlers{app: app, logger: logger}
	mux.HandleFunc("GET /api/records/custom", h.list)
	mux.HandleFunc("POST /api/records/custom", h.create)
	mux.HandleFunc("DELETE /api/records/custom/{id}", h.del)
}

type customRecordHandlers struct {
	app    *App
	logger *slog.Logger
}

func (h *customRecordHandlers) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	list, err := h.app.CustomBestEfforts.List(r.Context(), userID)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		out = append(out, customBestEffortJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"custom_best_efforts": out})
}

func (h *customRecordHandlers) create(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
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
	created, err := h.app.CustomBestEfforts.Create(r.Context(), domain.CustomBestEffort{
		UserID: userID, ActivityType: domain.ActivityType(body.ActivityType),
		Metric: domain.BestEffortMetric(body.Metric), WindowKind: domain.BestEffortWindowKind(body.WindowKind),
		WindowValue: body.WindowValue, Label: body.Label,
	})
	if err != nil {
		writeActionError(w, err, "create failed")
		return
	}
	backfillCustomRecords(h.app, h.logger, userID, created.ActivityType)
	writeJSON(w, http.StatusCreated, map[string]any{"custom_best_effort": customBestEffortJSON(created)})
}

func (h *customRecordHandlers) del(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.app.CustomBestEfforts.Delete(r.Context(), userID, domain.CustomBestEffortID(id)); err != nil {
		writeActionError(w, err, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// backfillCustomRecords recomputes the user's efforts of one type in the
// background so the request returns at once; the outcome is logged.
func backfillCustomRecords(app *App, logger *slog.Logger, userID domain.UserID, at domain.ActivityType) {
	goSafe(logger, "custom best-effort backfill", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		sources, failed, err := app.CustomBestEfforts.Backfill(ctx, userID, at)
		logger.Info("custom best-effort backfill finished", "user_id", userID, "activity_type", at,
			"sources", sources, "failed", failed, "error", err)
	})
}

func customBestEffortJSON(c domain.CustomBestEffort) map[string]any {
	return map[string]any{
		"id": c.ID.String(), "activity_type": string(c.ActivityType), "metric": string(c.Metric),
		"window_kind": string(c.WindowKind), "window_value": c.WindowValue, "label": c.Label,
		"created_at": c.CreatedAt.UTC().Format(time.RFC3339),
	}
}
