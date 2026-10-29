package main

import (
	"log/slog"
	"net/http"
	"strconv"
)

// mountHealth serves merged daily health metrics (raw per-provider samples
// merged per type/day by the provider cascade). Empty until a health worker imports.
//
//	GET /api/health?type=HRV&days=90
func mountHealth(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Health == nil {
		return
	}
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		dt := r.URL.Query().Get("type")
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		merged, err := app.HealthViews.Series(r.Context(), userID, dt, days)
		if err != nil {
			logger.Error("list health samples failed", "user_id", userID, "type", dt, "error", err)
			writeActionError(w, err, "load failed")
			return
		}
		out := make([]map[string]any, 0, len(merged))
		for _, m := range merged {
			out = append(out, map[string]any{
				"day":      m.Day.Format("2006-01-02"),
				"value":    m.Value,
				"unit":     m.Unit,
				"provider": m.Provider,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"type": dt, "series": out})
	})
}
