package rest

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/dashboard"
)

// DashboardHandlers serve the owner's dashboards.
//
//	GET /api/overview            landing dashboard
//	GET /api/stats               stats page
//	GET /api/analysis?days=N     training load (default 180 days)
//	GET /api/activities/feed     faceted activity list (dash_feed.go)
//	GET /api/activities/heatmap  every GPS track (dash_feed.go)
type DashboardHandlers struct {
	Auth      *Auth
	Overviews *dashboard.Overviews
	Searches  *dashboard.Searches
	Stats     *dashboard.Stats
	Log       *slog.Logger
}

// Mount registers the routes.
func (h *DashboardHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/overview", h.overview)
	mux.HandleFunc("GET /api/analysis", h.analysis)
	mux.HandleFunc("GET /api/stats", h.stats)
	mux.HandleFunc("GET /api/activities/feed", h.feed)
	mux.HandleFunc("GET /api/activities/heatmap", h.heatmap)
}

// overview → { totals, by_sport:[{value,count}], recent:[activity...], this_week, last_week }
func (h *DashboardHandlers) overview(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	ov, err := h.Overviews.Get(r.Context(), userID)
	if err != nil {
		http.Error(w, "totals failed", http.StatusInternalServerError)
		return
	}
	rec := make([]dashFeedActivity, 0, len(ov.Recent))
	for _, a := range ov.Recent {
		rec = append(rec, dashFeedActivityOf(a, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"totals":    dashTotalsJSON(ov.Totals),
		"by_sport":  dashFacets(ov.BySport),
		"recent":    rec,
		"this_week": dashTotalsJSON(ov.ThisWeek),
		"last_week": dashTotalsJSON(ov.LastWeek),
	})
}

func dashTotalsJSON(t port.ActivityTotalsResult) map[string]any {
	return map[string]any{
		"count": t.Count, "distance_m": t.DistanceM,
		"moving_s": t.MovingS, "elevation_gain_m": t.ElevationGainM,
	}
}

// stats → { totals, by_sport, by_discipline, by_year, records:{longest_distance, longest_duration} }
func (h *DashboardHandlers) stats(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	st, err := h.Stats.Get(r.Context(), userID)
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
		"totals":        dashTotalsJSON(st.Totals),
		"by_sport":      dashFacets(st.BySport),
		"by_discipline": dashFacets(st.ByDiscipline),
		"by_year":       yearOut,
		"records": map[string]any{
			"longest_distance": dashStatsRecordJSON(st.LongestDistance),
			"longest_duration": dashStatsRecordJSON(st.LongestDuration),
		},
	})
}

// dashStatsRecordJSON renders a record-holding activity; nil when none.
func dashStatsRecordJSON(a *domain.Activity) map[string]any {
	if a == nil {
		return nil
	}
	var dist float64
	if a.Summary.DistanceM != nil {
		dist = *a.Summary.DistanceM
	}
	return map[string]any{
		"id": a.ID.String(), "title": a.Title,
		"type": string(a.Type), "discipline": string(a.Discipline),
		"distance_m":         dist,
		"elapsed_duration_s": int64(a.ElapsedDuration.Seconds()),
	}
}

// analysis → { dates, ctl, atl, tsb, tss, current:{ctl,atl,tsb} }
func (h *DashboardHandlers) analysis(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	series, err := h.Stats.TrainingLoad(r.Context(), userID, days)
	if err != nil {
		writeActionError(w, err, "analysis failed")
		return
	}
	writeJSON(w, http.StatusOK, dashAnalysisJSON(series))
}

// dashAnalysisJSON renders the per-day series as parallel arrays.
func dashAnalysisJSON(series []domain.TrainingLoadDay) map[string]any {
	dates := make([]string, 0, len(series))
	ctl := make([]float64, 0, len(series))
	atl := make([]float64, 0, len(series))
	tsb := make([]float64, 0, len(series))
	tss := make([]float64, 0, len(series))
	for _, d := range series {
		dates = append(dates, d.Date)
		ctl = append(ctl, d.CTL)
		atl = append(atl, d.ATL)
		tsb = append(tsb, d.TSB)
		tss = append(tss, d.TSS)
	}
	current := map[string]any{"ctl": 0.0, "atl": 0.0, "tsb": 0.0}
	if n := len(series); n > 0 {
		current = map[string]any{"ctl": series[n-1].CTL, "atl": series[n-1].ATL, "tsb": series[n-1].TSB}
	}
	return map[string]any{
		"dates": dates, "ctl": ctl, "atl": atl, "tsb": tsb, "tss": tss,
		"current": current,
	}
}
