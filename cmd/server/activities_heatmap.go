package main

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// activities_heatmap serves every GPS track the owner's activities have, in
// one response, for the combined "all my tracks" map.
//
//	GET /api/activities/heatmap
//	  ?<the standard activity filter — see activities_feed.go>
//	  &res=30s|5s                 stream resolution (default 30s)
//	  → { matched, with_track, truncated, facets:{...},
//	      geojson: FeatureCollection<LineString>{properties:{id,type,discipline}} }
//
// Owner-only: the viewer's own activities, so no privacy-zone trimming.
func mountActivitiesHeatmap(mux *http.ServeMux, app *App, logger *slog.Logger) {
	mux.HandleFunc("GET /api/activities/heatmap", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		res := domain.StreamResolution30s
		if q.Get("res") == string(domain.StreamResolution5s) {
			res = domain.StreamResolution5s
		}
		hm, err := app.Stats.Heatmap(r.Context(), userID, parseActivityFilter(q), res)
		if err != nil {
			logger.Error("heatmap failed", "error", err)
			writeActionError(w, err, "tracks failed")
			return
		}
		years := make([]feedFacet, 0, len(hm.Years))
		for _, y := range hm.Years {
			years = append(years, feedFacet{Value: strconv.Itoa(y.Year), Count: y.Count})
		}
		features := make([]map[string]any, 0, len(hm.Tracks))
		for _, t := range hm.Tracks {
			features = append(features, heatmapFeature(t))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"matched":    hm.Matched,
			"with_track": len(features),
			"truncated":  hm.Truncated,
			"facets": map[string]any{
				"types":       toFeedFacets(hm.Types),
				"disciplines": toFeedFacets(hm.Disciplines),
				"years":       years,
			},
			"geojson": map[string]any{"type": "FeatureCollection", "features": features},
		})
	})

	logger.Info("activities heatmap endpoint mounted", "path", "/api/activities/heatmap")
}

// heatmapFeature builds one LineString feature at ~1 m precision.
func heatmapFeature(t view.HeatmapTrack) map[string]any {
	coords := make([][2]float64, len(t.Points))
	for i, p := range t.Points {
		coords[i] = [2]float64{round5(p.Lon), round5(p.Lat)}
	}
	return map[string]any{
		"type": "Feature",
		"id":   t.Activity.ID.String(),
		"properties": map[string]any{
			"id":         t.Activity.ID.String(),
			"type":       string(t.Activity.Type),
			"discipline": string(t.Activity.Discipline),
		},
		"geometry": map[string]any{"type": "LineString", "coordinates": coords},
	}
}

func round5(v float64) float64 { return math.Round(v*1e5) / 1e5 }
