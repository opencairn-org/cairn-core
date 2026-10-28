package main

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// mountAnalysis powers the Analysis page: the daily training-load series
// (CTL = fitness, ATL = fatigue, TSB = form) plus daily TSS, for tracking real
// fitness progress over time.
//
//	GET /api/analysis?days=N   (default 180)
//	  → { dates:[...], ctl:[...], atl:[...], tsb:[...], tss:[...],
//	      current:{ctl,atl,tsb} }
func mountAnalysis(mux *http.ServeMux, app *App, logger *slog.Logger) {
	mux.HandleFunc("GET /api/analysis", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		series, err := app.Stats.TrainingLoad(r.Context(), userID, days)
		if err != nil {
			writeActionError(w, err, "analysis failed")
			return
		}
		writeJSON(w, http.StatusOK, analysisSeriesJSON(series))
	})

	logger.Info("analysis endpoint mounted")
}

// analysisSeriesJSON renders the per-day series as parallel arrays.
func analysisSeriesJSON(series []domain.TrainingLoadDay) map[string]any {
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
