package main

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// mountActivitySimilar exposes "activities like this one" — other activities
// that cover roughly the same route (shared segment efforts) — plus the metrics
// the progression view needs (domain.BuildSimilarGroup via usecase/view).
//
//	GET /api/activities/{id}/similar
//	  → { target_segments, activities:[{id,is_current,title,type,start_time,
//	         distance_m,moving_s,elapsed_s,avg_heart_rate,avg_power,pace_s_per_km,
//	         shared_segments,overlap_pct}],
//	      summary:{count,best_moving_s,current_moving_s,current_rank,is_best} }
func mountActivitySimilar(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Segments == nil {
		return
	}
	mux.HandleFunc("GET /api/activities/{id}/similar", func(w http.ResponseWriter, r *http.Request) {
		act, ok := activityManageLoadOwned(w, r, app)
		if !ok {
			return
		}
		g, err := app.Details.Similar(r.Context(), act)
		if err != nil {
			logger.Error("find similar failed", "activity", act.ID, "error", err)
			writeActionError(w, err, "load failed")
			return
		}
		out := make([]map[string]any, 0, len(g.Rows))
		for _, row := range g.Rows {
			out = append(out, similarRowJSON(row))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"target_segments": g.TargetSegments,
			"activities":      out,
			"summary": map[string]any{
				"count":            len(g.Rows),
				"best_moving_s":    g.BestMovingS,
				"current_moving_s": g.CurrentMovingS,
				"current_rank":     g.CurrentRank,
				"is_best":          g.IsBest,
			},
		})
	})
	logger.Info("activity similar endpoint mounted")
}

func similarRowJSON(row domain.SimilarRow) map[string]any {
	return map[string]any{
		"id":              row.ActivityID.String(),
		"is_current":      row.IsCurrent,
		"title":           row.Title,
		"type":            row.Type,
		"start_time":      row.StartTime.UTC(),
		"distance_m":      row.DistanceM,
		"moving_s":        row.MovingS,
		"elapsed_s":       row.ElapsedS,
		"avg_heart_rate":  row.AvgHeartRateBpm,
		"avg_power":       row.AvgPowerW,
		"pace_s_per_km":   row.PaceSPerKm,
		"shared_segments": row.SharedSegments,
		"overlap_pct":     row.OverlapPct,
	}
}
