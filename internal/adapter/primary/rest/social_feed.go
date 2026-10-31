package rest

import (
	"net/http"
	"time"

	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// HomeFeedHandlers serve the logged-in landing feed: own activities, the local
// following feed (projected) and federated activities, newest first.
//
//	GET /api/feed/home
type HomeFeedHandlers struct {
	Auth  *Auth
	Feeds *view.Feeds
}

// Mount registers the route.
func (h *HomeFeedHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/feed/home", h.home)
}

func (h *HomeFeedHandlers) home(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	limit := parseIntQuery(r, "limit", view.FeedDefaultLimit)
	offset := parseIntQuery(r, "offset", 0)
	items := h.Feeds.Home(r.Context(), me, limit, offset)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, socialFeedItemJSON(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"activities": out})
}

// socialFeedItemJSON renders one home-feed row in the REST shape.
func socialFeedItemJSON(it view.FeedItem) map[string]any {
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
