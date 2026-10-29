package main

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// mountSegmentsList exposes the segments landing page data: a headline status
// block + the viewer's most-attempted segments (view.Segments.Mine).
//
//	GET /api/segments?type=<activity_type>&limit=&offset=
func mountSegmentsList(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Segments == nil {
		return
	}
	mux.HandleFunc("GET /api/segments", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		limit, offset := segmentsListPaging(r)
		page, err := app.SegmentViews.Mine(r.Context(), userID, r.URL.Query().Get("type"), limit, offset)
		if err != nil {
			logger.Error("list segments failed", "user", userID, "error", err)
			writeActionError(w, err, "load failed")
			return
		}
		var stats domain.UserSegmentStats
		if page.Stats != nil {
			stats = *page.Stats
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"stats": map[string]any{
				"segments": stats.Segments,
				"efforts":  stats.Efforts,
				"prs":      stats.PRs,
				"crs":      stats.CRs,
				"external": stats.External,
				"native":   stats.Native,
			},
			"segments": segmentsListItemsJSON(page.Items),
			"limit":    limit,
			"offset":   offset,
			"has_more": page.HasMore,
		})
	})

	logger.Info("segments list endpoint mounted")
}

// segmentsListPaging parses limit (default 50, max 200) and offset.
func segmentsListPaging(r *http.Request) (limit, offset int) {
	limit = 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 200)
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	return limit, offset
}

// segmentsListItemsJSON renders the segment rows.
func segmentsListItemsJSON(items []domain.UserSegmentListItem) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"id":               it.ID.String(),
			"name":             it.Name,
			"activity_type":    it.ActivityType,
			"source":           string(it.Source),
			"distance_m":       it.DistanceM,
			"elevation_gain_m": it.ElevationGainM,
			"avg_grade":        it.AvgGrade,
			"effort_count":     it.EffortCount,
			"best_elapsed_s":   it.BestElapsedS,
			"last_effort_at":   it.LastEffortAt.UTC(),
			"has_pr":           it.HasPR,
			"has_cr":           it.HasCR,
		})
	}
	return out
}
