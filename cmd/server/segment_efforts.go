package main

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// mountSegmentEfforts exposes an activity's segment efforts (joined to their
// segment) as JSON for the activity-detail page.
//
//	GET /api/activities/{id}/segment-efforts
//	  → { efforts: [{ segment_name, segment_distance_m, climb_category,
//	                  elapsed_s, moving_s, avg_heart_rate, avg_power,
//	                  is_personal_record, personal_rank, start_offset_s, … }] }
func mountSegmentEfforts(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Segments == nil {
		return
	}
	mux.HandleFunc("GET /api/activities/{id}/segment-efforts", func(w http.ResponseWriter, r *http.Request) {
		act, ok := activityManageLoadOwned(w, r, app)
		if !ok {
			return
		}
		rows, err := app.Details.SegmentEfforts(r.Context(), act)
		if err != nil {
			writeActionError(w, err, "load failed")
			return
		}
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			out = append(out, segmentEffortRowJSON(row))
		}
		writeJSON(w, http.StatusOK, map[string]any{"efforts": out})
	})
	logger.Info("segment-efforts endpoint mounted")
}

// segmentEffortRowJSON renders one effort, located on the stream when possible.
func segmentEffortRowJSON(row view.EffortRow) map[string]any {
	e, seg := row.Effort, row.Segment
	out := map[string]any{
		"segment_id":         e.SegmentID.String(),
		"segment_name":       seg.Name,
		"segment_distance_m": seg.DistanceM,
		"climb_category":     segmentClimbJSON(seg),
		"elapsed_s":          e.ElapsedS,
		"moving_s":           e.MovingS,
		"avg_heart_rate":     e.AvgHeartRateBpm,
		"avg_power":          e.AvgPowerW,
		"is_personal_record": e.IsPersonalRecord,
		"personal_rank":      e.PersonalRank,
	}
	if row.StartOffsetS != nil {
		out["start_offset_s"] = *row.StartOffsetS
		out["end_offset_s"] = *row.EndOffsetS
	}
	if row.StartDistanceM != nil {
		out["start_distance_m"] = *row.StartDistanceM
	}
	if row.EndDistanceM != nil {
		out["end_distance_m"] = *row.EndDistanceM
	}
	return out
}

var _ = domain.SegmentEffort{}
