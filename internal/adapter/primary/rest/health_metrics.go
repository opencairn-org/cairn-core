package rest

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/opencairn-org/cairn-core/internal/usecase/dashboard"
)

// HealthMetricsHandlers serve merged daily health metrics (per-provider
// samples merged per type/day by the provider cascade).
//
//	GET /api/health?type=HRV&days=90
type HealthMetricsHandlers struct {
	Auth   *Auth
	Health *dashboard.Health
	Log    *slog.Logger
}

// Mount registers the routes.
func (h *HealthMetricsHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", h.series)
}

func (h *HealthMetricsHandlers) series(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	dt := r.URL.Query().Get("type")
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	merged, err := h.Health.Series(r.Context(), userID, dt, days)
	if err != nil {
		h.Log.Error("list health samples failed", "user_id", userID, "type", dt, "error", err)
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
}

// InstanceFeaturesHandlers tell the SPA which optional instance features are
// on. Public on purpose: only booleans an anonymous visitor could infer anyway.
//
//	GET /api/instance/features → { "federation": bool }
type InstanceFeaturesHandlers struct {
	Federation bool
}

// Mount registers the routes.
func (h *InstanceFeaturesHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/instance/features", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, http.StatusOK, map[string]any{"federation": h.Federation})
	})
}
