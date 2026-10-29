package main

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// mountLaps exposes an activity's provider-reported laps (splits) for the
// activity-detail page. Laps live on the source payload; the merge engine picks
// one source's lap segmentation, so we surface the laps from whichever attached
// source has them (richest wins on ties).
//
//	GET /api/activities/{id}/laps
//	  → { laps: [{ index, label, start_offset_s, elapsed_s, moving_s,
//	              distance_m, avg_speed_mps, avg_heart_rate, avg_power,
//	              avg_cadence, elevation_gain_m }] }
func mountLaps(mux *http.ServeMux, app *App, logger *slog.Logger) {
	mux.HandleFunc("GET /api/activities/{id}/laps", func(w http.ResponseWriter, r *http.Request) {
		act, ok := activityManageLoadOwned(w, r, app)
		if !ok {
			return
		}
		laps, _, err := app.Details.Laps(r.Context(), act)
		if err != nil {
			http.Error(w, "load failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"laps": lapsToJSON(laps)})
	})

	logger.Info("laps endpoint mounted")
}

// lapsToJSON renders laps for the laps endpoint, defaulting index to position.
func lapsToJSON(laps []domain.ActivityLap) []map[string]any {
	out := make([]map[string]any, 0, len(laps))
	for i, l := range laps {
		idx := l.Index
		if idx == 0 {
			idx = i + 1
		}
		out = append(out, map[string]any{
			"index":            idx,
			"label":            l.Label,
			"start_offset_s":   int64(l.StartOffset.Seconds()),
			"elapsed_s":        int64(l.ElapsedDuration.Seconds()),
			"moving_s":         int64(l.MovingDuration.Seconds()),
			"distance_m":       l.DistanceM,
			"avg_speed_mps":    l.AvgSpeedMps,
			"avg_heart_rate":   l.AvgHeartRateBpm,
			"avg_power":        l.AvgPowerW,
			"avg_cadence":      l.AvgCadence,
			"elevation_gain_m": l.ElevationGainM,
		})
	}
	return out
}
