package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// mountHomeFeed serves the logged-in landing feed: the user's own activities,
// the local following feed (projected through the visibility choke-point) and
// federated activities from followed remote actors, newest first.
func mountHomeFeed(mux *http.ServeMux, app *App, logger *slog.Logger) {
	mux.HandleFunc("GET /api/feed/home", func(w http.ResponseWriter, r *http.Request) {
		me, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		limit := parseIntQuery(r, "limit", view.FeedDefaultLimit)
		offset := parseIntQuery(r, "offset", 0)
		items := app.Feeds.Home(r.Context(), me, limit, offset)
		out := make([]map[string]any, 0, len(items))
		for _, it := range items {
			out = append(out, feedItemJSON(it))
		}
		writeJSON(w, http.StatusOK, map[string]any{"activities": out})
	})
	logger.Info("home feed endpoint mounted", "path", "/api/feed/home")
}

// feedItemJSON renders one feed row in the REST shape.
func feedItemJSON(it view.FeedItem) map[string]any {
	switch {
	case it.Own != nil:
		a := it.Own
		return map[string]any{
			"source":             "self",
			"id":                 a.ID.String(),
			"title":              a.Title,
			"type":               string(a.Type),
			"start_time":         a.StartTime.UTC().Format(time.RFC3339),
			"distance_m":         a.Summary.DistanceM,
			"elevation_gain_m":   a.Summary.ElevationGainM,
			"elapsed_duration_s": int64(a.ElapsedDuration.Seconds()),
			"start_place":        a.StartPlace,
			"map_image_url":      "/api/activities/" + a.ID.String() + "/map.png",
			"owner":              userCardJSON(it.Owner),
		}
	case it.Projected != nil:
		m := projectedJSON(*it.Projected)
		m["source"] = "following"
		m["owner"] = userCardJSON(it.Owner)
		return m
	case it.Federated != nil:
		f := it.Federated
		m := map[string]any{
			"source":           "federated",
			"id":               f.ActivityAPID,
			"title":            f.Name,
			"type":             f.Sport,
			"start_time":       f.Published.UTC().Format(time.RFC3339),
			"summary":          f.Summary,
			"url":              f.URL,
			"map_image_url":    f.ImageURL,
			"distance_m":       f.DistanceM,
			"elevation_gain_m": f.ElevationM,
			"owner": map[string]any{
				"display_name": view.RemoteHandle(f.ActorID),
				"remote":       true,
				"actor":        f.ActorID,
			},
		}
		if f.DurationS != nil {
			m["elapsed_duration_s"] = *f.DurationS
		}
		return m
	}
	return map[string]any{}
}
