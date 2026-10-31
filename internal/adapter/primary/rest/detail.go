package rest

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/streamalign"
	"github.com/opencairn-org/cairn-core/internal/usecase/activityexport"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// DetailHandlers serve the owner's per-activity detail reads.
//
//	GET /api/activities/{id}/laps            provider-reported laps (richest source)
//	GET /api/activities/{id}/zones           time-in-zone as of the activity's date   (Streams)
//	GET /api/activities/{id}/merged-stream   best source per channel on one grid       (Streams)
//	GET /api/activities/{id}/similar         activities over the same route            (Segments)
//	GET /api/activities/{id}/export.{gpx,tcx,fit}   interchange file of the merged activity
//	GET /api/activities/{id}/export/options  which export features the activity has
type DetailHandlers struct {
	Auth    *Auth
	Access  *view.ActivityAccess
	Details *view.Details
	Exports *activityexport.Exports
	Log     *slog.Logger
	// Streams / Segments gate the routes whose subsystem is wired.
	Streams, Segments bool
}

// Mount registers the routes.
func (h *DetailHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/activities/{id}/laps", h.laps)
	if h.Streams {
		mux.HandleFunc("GET /api/activities/{id}/zones", h.zones)
		mux.HandleFunc("GET /api/activities/{id}/merged-stream", h.mergedStream)
	}
	if h.Segments {
		mux.HandleFunc("GET /api/activities/{id}/similar", h.similar)
	}
	// Generated from the merged stream + summary, so it works whatever the
	// original format; distinct from the per-source "download original".
	for _, format := range []string{"gpx", "tcx", "fit"} {
		mux.HandleFunc("GET /api/activities/{id}/export."+format, h.exportFile(format))
	}
	mux.HandleFunc("GET /api/activities/{id}/export/options", h.exportOptions)
}

// owned resolves the caller's live {id} activity or writes the error.
func (h *DetailHandlers) owned(w http.ResponseWriter, r *http.Request) (domain.Activity, bool) {
	return detailOwned(w, r, h.Auth, h.Access, "bad activity id")
}

// detailOwned resolves the caller's live {id} activity; on failure it has
// written 401, 400 (badID) or 404.
func detailOwned(w http.ResponseWriter, r *http.Request, auth *Auth, access *view.ActivityAccess, badID string) (domain.Activity, bool) {
	me, ok := auth.requireUser(w, r)
	if !ok {
		return domain.Activity{}, false
	}
	id, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, badID, http.StatusBadRequest)
		return domain.Activity{}, false
	}
	act, err := access.Owned(r.Context(), me, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return domain.Activity{}, false
	}
	return act, true
}

func (h *DetailHandlers) laps(w http.ResponseWriter, r *http.Request) {
	act, ok := h.owned(w, r)
	if !ok {
		return
	}
	laps, _, err := h.Details.Laps(r.Context(), act)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"laps": detailLapsJSON(laps)})
}

// detailLapsJSON renders laps, defaulting index to position.
func detailLapsJSON(laps []domain.ActivityLap) []map[string]any {
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

func (h *DetailHandlers) zones(w http.ResponseWriter, r *http.Request) {
	act, ok := h.owned(w, r)
	if !ok {
		return
	}
	z, err := h.Details.Zones(r.Context(), act)
	if err != nil {
		writeActionError(w, err, "stream query failed")
		return
	}
	resp := map[string]any{"hr": detailZoneSummaryJSON(z.HR), "power": detailZoneSummaryJSON(z.Power)}
	if z.Decoupling != nil {
		resp["decoupling"] = map[string]any{"pct": z.Decoupling.Pct, "basis": z.Decoupling.Basis, "coupled": z.Decoupling.Coupled}
	}
	writeJSON(w, http.StatusOK, resp)
}

func detailZoneSummaryJSON(s *domain.ZoneSummary) any {
	if s == nil {
		return nil
	}
	zones := make([]map[string]any, len(s.Zones))
	for i, b := range s.Zones {
		zones[i] = map[string]any{"label": b.Label, "low_pct": b.LowPct, "high_pct": b.HighPct, "seconds": b.Seconds}
	}
	return map[string]any{"basis": s.Basis, "reference": s.Reference, "total_seconds": s.TotalSeconds, "zones": zones}
}

func (h *DetailHandlers) mergedStream(w http.ResponseWriter, r *http.Request) {
	act, ok := h.owned(w, r)
	if !ok {
		return
	}
	merged, has, err := h.Details.MergedStream(r.Context(), act)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	if !has {
		writeJSON(w, http.StatusOK, map[string]any{"grid": []int64{}, "channels": map[string]any{}})
		return
	}
	writeJSON(w, http.StatusOK, detailMergedStreamJSON(merged))
}

func detailMergedStreamJSON(merged streamalign.MergedStream) map[string]any {
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
	return map[string]any{"grid": grid, "channels": channels, "provenance": provenance}
}

func (h *DetailHandlers) similar(w http.ResponseWriter, r *http.Request) {
	act, ok := h.owned(w, r)
	if !ok {
		return
	}
	g, err := h.Details.Similar(r.Context(), act)
	if err != nil {
		h.Log.Error("find similar failed", "activity", act.ID, "error", err)
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(g.Rows))
	for _, row := range g.Rows {
		out = append(out, map[string]any{
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
		})
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
}

func (h *DetailHandlers) exportOptions(w http.ResponseWriter, r *http.Request) {
	act, ok := h.owned(w, r)
	if !ok {
		return
	}
	available, err := h.Exports.Available(r.Context(), act)
	if err != nil {
		writeActionError(w, err, "export options failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": available})
}

// exportFile serves the file; ?exclude=gps,heart_rate,… drops features.
func (h *DetailHandlers) exportFile(format string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		act, ok := h.owned(w, r)
		if !ok {
			return
		}
		file, err := h.Exports.Render(r.Context(), act, format, r.URL.Query().Get("exclude"))
		if err != nil {
			if _, isAction := actionErrorOf(err); !isAction {
				h.Log.Error("export failed", "activity", act.ID, "format", format, "error", err)
			}
			writeActionError(w, err, "export failed")
			return
		}
		detailWriteFile(w, file)
	}
}

// detailWriteFile writes an export as an attachment download.
func detailWriteFile(w http.ResponseWriter, file activityexport.ExportFile) {
	w.Header().Set("Content-Type", file.ContentType)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+file.Filename+"\"")
	w.Header().Set("Content-Length", strconv.Itoa(len(file.Data)))
	_, _ = w.Write(file.Data) //nolint:gosec // G705: non-HTML content type served as an attachment
}
