package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// mountSourceFromEndpoint registers "source data from another activity":
//
//	POST /api/activities/{id}/source-from   {donor_id, groups: [...]}
//
// It creates a DERIVED source on the target activity carrying the donor's
// selected field-group values (+ the donor's geo stream rebased to the
// target's start time when "gps_track" is requested), then pins those groups
// to the derived source via the existing manual-override mechanism. A recompute
// makes the derived values win the merge. The classic case: "rode the route but
// forgot my watch" — a manual activity (#2) that borrows a route from a donor.
//
// The derived payload Provides EXACTLY the selected groups, so it can never win
// a group the user didn't ask for (recency would otherwise let the newest
// source steal un-pinned groups).
func mountSourceFromEndpoint(mux *http.ServeMux, app *App, logger *slog.Logger) {
	h := &sourceFromAPI{app: app, logger: logger}
	mux.HandleFunc("POST /api/activities/{id}/source-from", h.handle)

	logger.Info("source-from endpoint mounted", "path", "/api/activities/{id}/source-from")
}

// sourceFromAPI holds the dependencies of the source-from handler.
type sourceFromAPI struct {
	app    *App
	logger *slog.Logger
}

// sourceFromRequest is a validated source-from request.
type sourceFromRequest struct {
	targetID   domain.ActivityID
	donorID    domain.ActivityID
	groups     []domain.FieldGroup
	copyStream bool
}

// parse validates the path + body or writes a 400.
func (h *sourceFromAPI) parse(w http.ResponseWriter, r *http.Request) (sourceFromRequest, bool) {
	var req sourceFromRequest
	tid, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid activity id", http.StatusBadRequest)
		return req, false
	}
	req.targetID = domain.ActivityID(tid)

	var body struct {
		DonorID string   `json:"donor_id"`
		Groups  []string `json:"groups"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return req, false
	}
	did, err := uuid.Parse(body.DonorID)
	if err != nil {
		http.Error(w, "invalid donor_id", http.StatusBadRequest)
		return req, false
	}
	req.donorID = domain.ActivityID(did)
	if req.donorID == req.targetID {
		http.Error(w, "donor must differ from the target activity", http.StatusBadRequest)
		return req, false
	}
	req.groups, req.copyStream, err = parseSourceGroups(body.Groups)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// loadPair loads the caller's target and donor activities or writes a 404.
func (h *sourceFromAPI) loadPair(ctx context.Context, w http.ResponseWriter, userID domain.UserID, req sourceFromRequest) (target, donor domain.Activity, ok bool) {
	target, err := h.app.Activities.GetActivity(ctx, req.targetID)
	if err != nil || target.UserID != userID || target.IsDeleted() {
		http.Error(w, "activity not found", http.StatusNotFound)
		return target, donor, false
	}
	donor, err = h.app.Activities.GetActivity(ctx, req.donorID)
	if err != nil || donor.UserID != userID || donor.IsDeleted() {
		http.Error(w, "donor activity not found", http.StatusNotFound)
		return target, donor, false
	}
	return target, donor, true
}

// sourceFromDerived builds the derived source. It Provides only the requested groups.
func sourceFromDerived(userID domain.UserID, req sourceFromRequest, donor domain.Activity) domain.ActivitySource {
	sid := domain.SourceID(uuid.New())
	now := time.Now().UTC()
	payload := domain.ActivitySourcePayload{}
	applySummaryGroups(&payload, donor.Summary, req.groups)
	if req.copyStream {
		payload.HasStream = true
	}
	return domain.ActivitySource{
		ID:                  sid,
		ActivityID:          req.targetID,
		UserID:              userID,
		Provider:            "derived",
		ExternalID:          uuid.NewString(),
		SourceWorkerName:    "derived",
		SourceWorkerVersion: "1.0",
		SourceWorkerPackage: "cairn",
		Parsed:              payload,
		Status:              domain.SourceStatusActive,
		StatusReason:        "derived from activity " + req.donorID.String(),
		ReimportStatus:      domain.ReimportStatusCurrent,
		ImportedAt:          now,
		UpdatedAt:           now,
	}
}

// pinAndRecompute pins the groups to the derived source and recomputes; it writes a 500 on failure.
func (h *sourceFromAPI) pinAndRecompute(ctx context.Context, w http.ResponseWriter, req sourceFromRequest, srcID domain.SourceID) bool {
	// Pin exactly the requested groups to the derived source.
	for _, g := range req.groups {
		if err := h.app.FieldOverrides.Set(ctx, domain.FieldSourceOverride{
			ActivityID: req.targetID, FieldKey: g, SourceID: srcID,
		}); err != nil {
			h.logger.Error("source-from: set override", "group", g, "error", err)
			http.Error(w, "set override failed", http.StatusInternalServerError)
			return false
		}
	}
	if _, err := h.app.RecomputeActivity.Execute(ctx, req.targetID); err != nil {
		h.logger.Error("source-from: recompute", "error", err)
		http.Error(w, "recompute failed", http.StatusInternalServerError)
		return false
	}
	return true
}

func (h *sourceFromAPI) handle(w http.ResponseWriter, r *http.Request) {
	app := h.app
	userID, ok := resolveSessionUser(r, app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if app.FieldOverrides == nil || app.RecomputeActivity == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	req, ok := h.parse(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	target, donor, ok := h.loadPair(ctx, w, userID, req)
	if !ok {
		return
	}

	src := sourceFromDerived(userID, req, donor)
	if err := app.Activities.SaveSource(ctx, src); err != nil {
		h.logger.Error("source-from: save source", "error", err)
		http.Error(w, "save source failed", http.StatusInternalServerError)
		return
	}

	// Borrow the donor's geo stream, rebased so its first sample aligns with
	// the target's start time.
	if req.copyStream {
		if err := borrowStream(ctx, app, donor, src.ID, target.StartTime); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if !h.pinAndRecompute(ctx, w, req, src.ID) {
		return
	}

	// Refresh the borrowed stream's downsampled aggregates (must run outside
	// a transaction) so charts/map read non-empty buckets.
	if req.copyStream {
		if err := app.Streams.RefreshAggregates(ctx, src.ID); err != nil {
			h.logger.Warn("source-from: refresh aggregates", "source_id", src.ID, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"source_id": src.ID.String()})
}

// parseSourceGroups validates the requested field-group keys and reports
// whether the geo stream should be borrowed (gps_track requested).
func parseSourceGroups(in []string) ([]domain.FieldGroup, bool, error) {
	if len(in) == 0 {
		return nil, false, fmt.Errorf("groups must list at least one field to source")
	}
	allowed := map[domain.FieldGroup]bool{
		domain.FieldGroupGPSTrack:    true,
		domain.FieldGroupDistance:    true,
		domain.FieldGroupElevation:   true,
		domain.FieldGroupSpeed:       true,
		domain.FieldGroupHeartRate:   true,
		domain.FieldGroupPower:       true,
		domain.FieldGroupCadence:     true,
		domain.FieldGroupTemperature: true,
		domain.FieldGroupCalories:    true,
	}
	var out []domain.FieldGroup
	copyStream := false
	for _, s := range in {
		g := domain.FieldGroup(s)
		if !allowed[g] {
			return nil, false, fmt.Errorf("field group %q cannot be sourced from another activity", s)
		}
		if g == domain.FieldGroupGPSTrack {
			copyStream = true
		}
		out = append(out, g)
	}
	return out, copyStream, nil
}

// applySummaryGroups copies the donor's summary values for the requested groups
// into the derived payload, leaving every other field nil so the source
// Provides only the selected groups.
func applySummaryGroups(p *domain.ActivitySourcePayload, donor domain.ActivitySummary, groups []domain.FieldGroup) {
	for _, g := range groups {
		switch g {
		case domain.FieldGroupDistance:
			p.Summary.DistanceM = donor.DistanceM
		case domain.FieldGroupElevation:
			p.Summary.ElevationGainM = donor.ElevationGainM
			p.Summary.ElevationLossM = donor.ElevationLossM
			p.Summary.MinElevationM = donor.MinElevationM
			p.Summary.MaxElevationM = donor.MaxElevationM
		case domain.FieldGroupSpeed:
			p.Summary.AvgSpeedMps = donor.AvgSpeedMps
			p.Summary.MaxSpeedMps = donor.MaxSpeedMps
		case domain.FieldGroupHeartRate:
			p.Summary.AvgHeartRateBpm = donor.AvgHeartRateBpm
			p.Summary.MaxHeartRateBpm = donor.MaxHeartRateBpm
		case domain.FieldGroupPower:
			p.Summary.AvgPowerW = donor.AvgPowerW
			p.Summary.MaxPowerW = donor.MaxPowerW
			p.Summary.NormalizedPowerW = donor.NormalizedPowerW
		case domain.FieldGroupCadence:
			p.Summary.AvgCadence = donor.AvgCadence
			p.Summary.MaxCadence = donor.MaxCadence
		case domain.FieldGroupTemperature:
			p.Summary.AvgTemperatureC = donor.AvgTemperatureC
			p.Summary.MinTemperatureC = donor.MinTemperatureC
			p.Summary.MaxTemperatureC = donor.MaxTemperatureC
		case domain.FieldGroupCalories:
			p.Summary.CaloriesKcal = donor.CaloriesKcal
		}
	}
}

// borrowStream copies the donor's primary stream onto the derived source,
// shifting every timestamp so the first sample aligns with the target's start.
func borrowStream(ctx context.Context, app *App, donor domain.Activity, dstSource domain.SourceID, targetStart time.Time) error {
	if donor.PrimaryStreamSourceID == nil {
		return fmt.Errorf("donor activity has no route to borrow")
	}
	src, err := app.Streams.QueryStream(ctx, domain.StreamQuery{
		ActivitySourceID: *donor.PrimaryStreamSourceID,
		Resolution:       domain.StreamResolutionRaw,
	})
	if err != nil {
		return fmt.Errorf("read donor stream: %w", err)
	}
	if len(src.Samples) == 0 {
		return fmt.Errorf("donor activity has no stream samples")
	}
	delta := targetStart.Sub(src.Samples[0].Timestamp)
	for i := range src.Samples {
		src.Samples[i].Timestamp = src.Samples[i].Timestamp.Add(delta)
	}
	if err := app.Streams.WriteStream(ctx, dstSource, src.Samples); err != nil {
		return fmt.Errorf("write borrowed stream: %w", err)
	}
	return nil
}
