// Package geocode contains the use case that reverse-geocodes activity start
// locations into the denormalised start_place subtitle ("Ride from Darmstadt").
//
// The use case is deliberately throttle-agnostic: the port.Geocoder enforces
// the provider's usage policy (1 req/s for OSM Nominatim), so the backfiller
// can call ExecuteForActivity in a tight loop and the geocoder paces it.
package geocode

import (
	"context"
	"errors"
	"log/slog"

	"github.com/opencairn-org/cairn-core/internal/port"
)

// ComputeStartPlace resolves an activity's start location from the first GPS
// point of its primary stream and stores the place name on the activity.
type ComputeStartPlace struct {
	activities port.ActivityGeoStore
	streams    port.StreamRepo
	geocoder   port.Geocoder
	logger     *slog.Logger
}

// NewComputeStartPlace wires the use case. logger may be nil (falls back to the
// default logger).
func NewComputeStartPlace(
	activities port.ActivityGeoStore,
	streams port.StreamRepo,
	geocoder port.Geocoder,
	logger *slog.Logger,
) *ComputeStartPlace {
	if logger == nil {
		logger = slog.Default()
	}
	return &ComputeStartPlace{
		activities: activities,
		streams:    streams,
		geocoder:   geocoder,
		logger:     logger.With("component", "geocode_start_place"),
	}
}

// ExecuteForActivity geocodes one candidate's start location.
//
// Outcomes (all persisted via SetStartLocation so the candidate drops out of
// the backfiller's queue):
//
//   - No GPS in the stream      → start_place = "" (attempted, no place)
//   - Geocoder found no place   → start_place = "" with the coords cached
//   - Place resolved            → start_place = name, coords cached
//
// A transport error from the geocoder is returned WITHOUT persisting anything,
// leaving start_place NULL so the next backfill tick retries it.
func (uc *ComputeStartPlace) ExecuteForActivity(ctx context.Context, c port.StartPlaceCandidate) error {
	lat, lon, found, err := uc.streams.FirstGeoPoint(ctx, c.PrimaryStreamSourceID)
	if err != nil {
		return err
	}
	if !found {
		// Indoor / no track — mark attempted so we stop reconsidering it.
		return uc.activities.SetStartLocation(ctx, c.ActivityID, nil, nil, "")
	}

	place, err := uc.geocoder.ReverseGeocode(ctx, lat, lon)
	if errors.Is(err, port.ErrNoPlace) {
		place, err = port.Place{}, nil
	}
	if err != nil {
		return err // transient — leave NULL, retry next tick
	}

	l, o := lat, lon
	return uc.activities.SetStartLocation(ctx, c.ActivityID, &l, &o, place.Name)
}

// BackfillBatch processes up to `limit` un-geocoded activities and returns how
// many it successfully resolved (place or "no place"). Per-activity transport
// errors are logged and skipped; they keep their NULL start_place and are
// retried on a later tick.
func (uc *ComputeStartPlace) BackfillBatch(ctx context.Context, limit int) (int, error) {
	cands, err := uc.activities.ListActivitiesMissingStartPlace(ctx, limit)
	if err != nil {
		return 0, err
	}
	resolved := 0
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return resolved, err
		}
		if err := uc.ExecuteForActivity(ctx, c); err != nil {
			uc.logger.Warn("geocode start place failed",
				"activity_id", c.ActivityID, "error", err)
			continue
		}
		resolved++
	}
	return resolved, nil
}
