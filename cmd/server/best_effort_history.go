package main

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// mountBestEffortHistory exposes the progression of one best-effort bucket
// (metric + window) across all the user's activities — the data behind the
// best-effort detail page (e.g. "Power 20 min" over time).
//
//	GET /api/best-efforts/history?type=&metric=&window_kind=&window_value=
//	  → { type, metric, window_kind, window_value, best, count, items:[{activity_id,title,start_time,achieved_value,is_best}] }
func mountBestEffortHistory(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.BestEfforts == nil {
		return
	}

	mux.HandleFunc("GET /api/best-efforts/history", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		activityType, metric, windowKind := q.Get("type"), q.Get("metric"), q.Get("window_kind")
		windowValue, _ := strconv.Atoi(q.Get("window_value"))
		h, err := app.Stats.BestEffortHistory(r.Context(), userID, activityType, metric, windowKind, windowValue)
		if err != nil {
			logger.Error("best-effort history failed", "user", userID, "error", err)
			writeActionError(w, err, "load failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"type":         activityType,
			"metric":       metric,
			"window_kind":  windowKind,
			"window_value": windowValue,
			"best":         h.Best,
			"count":        len(h.Items),
			"items":        bestEffortHistoryItemsJSON(h),
		})
	})

	logger.Info("best-effort history endpoint mounted")
}

// bestEffortHistoryItemsJSON renders the history items, flagging the best.
func bestEffortHistoryItemsJSON(h view.BestEffortHistory) []map[string]any {
	out := make([]map[string]any, 0, len(h.Items))
	for _, it := range h.Items {
		out = append(out, map[string]any{
			"activity_id":    it.ActivityID.String(),
			"title":          it.Title,
			"start_time":     it.StartTime.UTC(),
			"achieved_value": it.AchievedValue,
			"is_best":        it.AchievedValue == h.Best,
		})
	}
	return out
}
