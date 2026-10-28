package main

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// mountZones exposes per-activity time-in-zone (heart-rate + power), computed
// from the activity's stream and the athlete's thresholds resolved AS OF the
// activity's date (domain.ComputeZones via usecase/view.Details).
//
//	GET /api/activities/{id}/zones
//	  → { hr: {basis, reference, total_seconds, zones:[{label,low_pct,high_pct,seconds}]} | null,
//	      power: {...} | null, decoupling?: {pct, basis, coupled} }
func mountZones(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Streams == nil {
		return
	}
	mux.HandleFunc("GET /api/activities/{id}/zones", func(w http.ResponseWriter, r *http.Request) {
		act, ok := activityManageLoadOwned(w, r, app)
		if !ok {
			return
		}
		z, err := app.Details.Zones(r.Context(), act)
		if err != nil {
			writeActionError(w, err, "stream query failed")
			return
		}
		writeJSON(w, http.StatusOK, zonesJSON(z))
	})
	logger.Info("zones endpoint mounted")
}

// zonesJSON renders the zones result in the REST shape.
func zonesJSON(z domain.ZonesResult) map[string]any {
	resp := map[string]any{"hr": zoneSummaryJSON(z.HR), "power": zoneSummaryJSON(z.Power)}
	if z.Decoupling != nil {
		resp["decoupling"] = map[string]any{"pct": z.Decoupling.Pct, "basis": z.Decoupling.Basis, "coupled": z.Decoupling.Coupled}
	}
	return resp
}

func zoneSummaryJSON(s *domain.ZoneSummary) any {
	if s == nil {
		return nil
	}
	zones := make([]map[string]any, len(s.Zones))
	for i, b := range s.Zones {
		zones[i] = map[string]any{"label": b.Label, "low_pct": b.LowPct, "high_pct": b.HighPct, "seconds": b.Seconds}
	}
	return map[string]any{"basis": s.Basis, "reference": s.Reference, "total_seconds": s.TotalSeconds, "zones": zones}
}
