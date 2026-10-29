package main

import (
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain/streamalign"
)

// mountMergedStream serves the per-channel-merged, aligned, resampled stream:
// best source per channel across the activity's sources, on a common grid.
// Computed on demand from the raw streams (not persisted).
//
//	GET /api/activities/{id}/merged-stream
func mountMergedStream(mux *http.ServeMux, app *App) {
	if app.Streams == nil {
		return
	}
	mux.HandleFunc("GET /api/activities/{id}/merged-stream", func(w http.ResponseWriter, r *http.Request) {
		act, ok := activityManageLoadOwned(w, r, app)
		if !ok {
			return
		}
		merged, has, err := app.Details.MergedStream(r.Context(), act)
		if err != nil {
			writeActionError(w, err, "load failed")
			return
		}
		if !has {
			writeJSON(w, http.StatusOK, map[string]any{"grid": []int64{}, "channels": map[string]any{}})
			return
		}
		writeJSON(w, http.StatusOK, mergedStreamJSON(merged))
	})
}

// mergedStreamJSON builds the response body of a merged stream.
func mergedStreamJSON(merged streamalign.MergedStream) map[string]any {
	grid := make([]int64, len(merged.Grid))
	for i, t := range merged.Grid {
		grid[i] = t.Unix()
	}
	channels := map[string][]*float64{}
	provenance := map[string]string{}
	for ch, vals := range merged.Channels {
		channels[string(ch)] = vals
	}
	for ch, sid := range merged.Provenance {
		provenance[string(ch)] = sid.String()
	}
	return map[string]any{
		"grid":       grid,
		"channels":   channels,
		"provenance": provenance,
	}
}
