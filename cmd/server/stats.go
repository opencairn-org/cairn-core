package main

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// mountStats powers the Stats page — the data-geek view: lifetime totals,
// per-year breakdown, per-sport split, and personal records.
//
//	GET /api/stats
//	  → { totals, by_sport, by_year, records:{longest_distance, longest_duration} }
func mountStats(mux *http.ServeMux, app *App, logger *slog.Logger) {
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		st, err := app.Stats.Get(r.Context(), userID)
		if err != nil {
			http.Error(w, "stats failed", http.StatusInternalServerError)
			return
		}
		yearOut := make([]map[string]any, 0, len(st.ByYear))
		for _, y := range st.ByYear {
			yearOut = append(yearOut, map[string]any{
				"year": y.Year, "count": y.Count, "distance_m": y.DistanceM,
				"moving_s": y.MovingS, "elevation_gain_m": y.ElevationGainM,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"totals":        totalsJSON(st.Totals),
			"by_sport":      toFeedFacets(st.BySport),
			"by_discipline": toFeedFacets(st.ByDiscipline),
			"by_year":       yearOut,
			"records": map[string]any{
				"longest_distance": statsRecordJSON(st.LongestDistance),
				"longest_duration": statsRecordJSON(st.LongestDuration),
			},
		})
	})

	logger.Info("stats endpoint mounted")
}

// statsRecordJSON renders a record-holding activity; nil when none.
func statsRecordJSON(a *domain.Activity) map[string]any {
	if a == nil {
		return nil
	}
	return map[string]any{
		"id": a.ID.String(), "title": a.Title,
		"type": string(a.Type), "discipline": string(a.Discipline),
		"distance_m":         deref(a.Summary.DistanceM),
		"elapsed_duration_s": int64(a.ElapsedDuration.Seconds()),
	}
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
