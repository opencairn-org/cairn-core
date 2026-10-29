package main

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/port"
)

// mountOverview powers the landing dashboard: lifetime totals, a per-sport
// breakdown, and the latest activities.
//
//	GET /api/overview
//	  → { totals:{count,distance_m,moving_s,elevation_gain_m},
//	      by_sport:[{value,count}], recent:[activity...] }
func mountOverview(mux *http.ServeMux, app *App, logger *slog.Logger) {
	mux.HandleFunc("GET /api/overview", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		ov, err := app.Overviews.Get(r.Context(), userID)
		if err != nil {
			http.Error(w, "totals failed", http.StatusInternalServerError)
			return
		}
		rec := make([]feedActivity, 0, len(ov.Recent))
		for _, a := range ov.Recent {
			rec = append(rec, feedActivityOf(a, false))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"totals":    totalsJSON(ov.Totals),
			"by_sport":  toFeedFacets(ov.BySport),
			"recent":    rec,
			"this_week": totalsJSON(ov.ThisWeek),
			"last_week": totalsJSON(ov.LastWeek),
		})
	})

	logger.Info("overview endpoint mounted")
}

func totalsJSON(t port.ActivityTotalsResult) map[string]any {
	return map[string]any{
		"count": t.Count, "distance_m": t.DistanceM,
		"moving_s": t.MovingS, "elevation_gain_m": t.ElevationGainM,
	}
}
