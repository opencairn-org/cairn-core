package activity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// IngestActivityFromWorker is the entry point for new activity data arriving
// from a worker. The worker has already parsed provider data into an
// ActivitySourcePayload; this use case resolves the source's IDENTITY and
// persists it, then the caller runs the re-clustering engine
// (usecase/match.ReclusterBucket) to decide same-activity grouping.
//
// Identity resolution:
//
//	exact match on (provider, external_account_id, external_id)
//	    → reimport: replace the existing source's parsed payload in place.
//	otherwise
//	    → persist the source as its own brand-new singleton activity.
//
// "Which records are the same real-world activity" is NOT decided here — that is
// the matcher's job (fuzzy scoring + union-find + stable-id reconciliation),
// kept a pure, re-runnable decision over the archived sources rather than an
// irreversible attach-on-ingest. There is no legacy heuristic dedup path.
type IngestActivityFromWorker struct {
	activities port.ActivityRepo
	streams    port.StreamRepo
	settings   port.UserSettingsRepo
	recompute  *RecomputeActivityFromSources
	tx         port.TxManager

	now   func() time.Time
	newID func() uuid.UUID
}

// NewIngestActivityFromWorker wires the use case. now and newID default to
// time.Now and uuid.NewV7 when nil. streams may be nil — callers that include
// Stream samples then get an error.
func NewIngestActivityFromWorker(
	activities port.ActivityRepo,
	streams port.StreamRepo,
	settings port.UserSettingsRepo,
	recompute *RecomputeActivityFromSources,
	tx port.TxManager,
	now func() time.Time,
	newID func() uuid.UUID,
) *IngestActivityFromWorker {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if newID == nil {
		newID = func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				// uuid.NewV7 only errors on system clock issues; fall back
				// to v4 which is still time-orderedish via the OS RNG.
				return uuid.New()
			}
			return id
		}
	}
	return &IngestActivityFromWorker{
		activities: activities,
		streams:    streams,
		settings:   settings,
		recompute:  recompute,
		tx:         tx,
		now:        now,
		newID:      newID,
	}
}

// IngestInput is the full input bundle handed to the use case. The
// adapter layer (worker control plane) constructs it from a Report RPC
// payload.
type IngestInput struct {
	UserID            domain.UserID
	Provider          string
	ExternalAccountID *domain.ExternalAccountID
	ExternalID        string

	SourceWorkerName    string
	SourceWorkerVersion string
	SourceWorkerPackage string
	SourceWorkerRelease string

	RawBlobID      string
	RawContentType string
	RawSizeBytes   int64

	Payload domain.ActivitySourcePayload

	// Stream is the optional time-series the worker parsed alongside
	// Payload. When non-nil, it replaces any existing stream for the
	// resulting ActivitySource, atomically within the ingest transaction.
	// An empty (non-nil) slice clears the stream — used by parsing
	// workers that detect "this provider had no stream after all".
	Stream []domain.StreamSample
}

// IngestAction reports which identity branch matched. Same-activity GROUPING is
// reported separately by ReclusterBucket, not here.
type IngestAction string

const (
	IngestActionImportedNew        IngestAction = "imported_new"
	IngestActionReimportedExisting IngestAction = "reimported_existing"
	// IngestActionMergedIntoExisting is retained for downstream callers
	// (import-history classification); the merge itself now happens in the
	// re-cluster step, not in ingest.
	IngestActionMergedIntoExisting IngestAction = "merged_into_existing"
)

// IngestResult is what the adapter returns to the worker after Report.
type IngestResult struct {
	ActivityID domain.ActivityID
	SourceID   domain.SourceID
	Action     IngestAction
	Activity   domain.Activity
}

// Execute resolves the source's identity and persists it (reimport replace, or a
// new singleton activity). The caller then runs ReclusterBucket to group it with
// any same-activity siblings.
func (uc *IngestActivityFromWorker) Execute(ctx context.Context, in IngestInput) (IngestResult, error) {
	if err := validateIngestInput(in); err != nil {
		return IngestResult{}, err
	}
	var result IngestResult
	err := uc.tx.InTx(ctx, func(ctx context.Context) error {
		existing, err := uc.activities.FindSourceByExternalID(
			ctx, in.Provider, in.ExternalAccountID, in.ExternalID,
		)
		if err == nil {
			return uc.handleReimport(ctx, existing, in, &result)
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("ingest identity lookup: %w", err)
		}
		return uc.handleCreateNew(ctx, in, &result)
	})
	// A first merge attaches gear outside Recompute (the reimport branch
	// goes through it), so refresh totals here for that case.
	if err == nil && result.Action == IngestActionImportedNew && result.Activity.GearID != nil && uc.recompute != nil {
		uc.recompute.refreshGearTotals(ctx, result.Activity.UserID)
	}
	return result, err
}

// ---------------------------------------------------------------------------
// branches
// ---------------------------------------------------------------------------

func (uc *IngestActivityFromWorker) handleReimport(
	ctx context.Context,
	existing domain.ActivitySource,
	in IngestInput,
	result *IngestResult,
) error {
	// Replace the parsed payload and provenance fields. ID, ActivityID,
	// UserID, Provider, ExternalAccountID, ExternalID, and ImportedAt
	// are preserved — they are the source's identity.
	if err := domain.CheckParserForward(existing, in.SourceWorkerPackage, in.SourceWorkerVersion); err != nil {
		return err
	}
	updated := existing
	updated.Parsed = in.Payload
	// An unknown gear lookup must not clear gear a previous import had.
	if in.Payload.GearUnknown && in.Payload.GearExternalID == "" && existing.Parsed.GearExternalID != "" {
		updated.Parsed.GearExternalID = existing.Parsed.GearExternalID
		updated.Parsed.GearName = existing.Parsed.GearName
	}
	updated.SourceWorkerName = in.SourceWorkerName
	updated.SourceWorkerVersion = in.SourceWorkerVersion
	updated.SourceWorkerPackage = in.SourceWorkerPackage
	updated.SourceWorkerRelease = in.SourceWorkerRelease
	updated.RawBlobID = in.RawBlobID
	// Preserve existing blob metadata when a re-push doesn't supply it (e.g. a
	// reparse that reuses the archived blob without re-stating its size/type).
	if in.RawContentType != "" {
		updated.RawContentType = in.RawContentType
	}
	if in.RawSizeBytes > 0 {
		updated.RawSizeBytes = in.RawSizeBytes
	}
	updated.Status = domain.SourceStatusActive
	updated.StatusReason = ""
	updated.ReimportStatus = domain.ReimportStatusCurrent
	updated.ReimportStatusReason = ""
	if lat, lon, ok := firstGeoPoint(in.Stream); ok {
		updated.StartLat, updated.StartLng = &lat, &lon
	}
	now := uc.now()
	updated.LastReimportedAt = &now
	updated.UpdatedAt = now

	if err := uc.activities.SaveSource(ctx, updated); err != nil {
		return fmt.Errorf("save reimported source: %w", err)
	}

	if err := uc.writeStreamIfPresent(ctx, existing.ID, in); err != nil {
		return err
	}

	rec, err := uc.recompute.Execute(ctx, existing.ActivityID)
	if err != nil {
		return fmt.Errorf("recompute after reimport: %w", err)
	}

	*result = IngestResult{
		ActivityID: existing.ActivityID,
		SourceID:   existing.ID,
		Action:     IngestActionReimportedExisting,
		Activity:   rec.Activity,
	}
	return nil
}

func (uc *IngestActivityFromWorker) handleCreateNew(
	ctx context.Context,
	in IngestInput,
	result *IngestResult,
) error {
	activityID := domain.ActivityID(uc.newID())
	src := uc.buildSource(activityID, in)

	// Run the merge engine on the single source so we have a fully
	// computed Activity to insert. We use a resolver that calls the
	// user's settings; on errors it falls back to defaults (same
	// behaviour as RecomputeActivityFromSources).
	resolver := func(t domain.ActivityType) domain.MergePolicy {
		p, err := uc.settings.GetMergePolicy(ctx, in.UserID, t)
		if err != nil {
			return domain.DefaultMergePolicyFor(t)
		}
		return p
	}

	merged, err := domain.Merge(activityID, []domain.ActivitySource{src}, resolver, uc.now())
	if err != nil {
		return fmt.Errorf("initial merge: %w", err)
	}
	merged.CreatedAt = uc.now()
	if uc.recompute != nil {
		if err := uc.recompute.assignGear(ctx, &merged, nil, []domain.ActivitySource{src}, nil, resolver); err != nil {
			return fmt.Errorf("initial gear: %w", err)
		}
	}
	if err := merged.Validate(); err != nil {
		return fmt.Errorf("initial activity invalid: %w", err)
	}

	if err := uc.activities.SaveActivity(ctx, merged); err != nil {
		return fmt.Errorf("save new activity: %w", err)
	}
	if err := uc.activities.SaveSource(ctx, src); err != nil {
		return fmt.Errorf("save initial source: %w", err)
	}

	if err := uc.writeStreamIfPresent(ctx, src.ID, in); err != nil {
		return err
	}

	// Denormalise the start coordinate on the activity (from the in-memory
	// stream, no DB read) so reverse-geocoding has a point immediately.
	// Best-effort: the async geocoder is the backstop, so a failure here must
	// not roll back the import.
	if lat, lon, ok := firstGeoPoint(in.Stream); ok {
		_ = uc.activities.SetStartCoords(ctx, activityID, lat, lon)
	}

	*result = IngestResult{
		ActivityID: activityID,
		SourceID:   src.ID,
		Action:     IngestActionImportedNew,
		Activity:   merged,
	}
	return nil
}

// writeStreamIfPresent persists the worker-supplied stream (when present)
// onto the new or updated source. Called from each of the ingest branches
// inside the transaction so success-or-rollback is atomic with the source
// write.
//
// in.Stream == nil means "the worker didn't include a stream this time"
// — leave any existing stream untouched (typical reimport from a worker
// that doesn't re-parse the raw blob). An empty (non-nil) slice means
// "the worker explicitly says there is no stream" — clear any existing.
//
// When the input includes a stream but no StreamRepo is wired, this is a
// programmer error and we return an explicit message.
func (uc *IngestActivityFromWorker) writeStreamIfPresent(
	ctx context.Context,
	sourceID domain.SourceID,
	in IngestInput,
) error {
	if in.Stream == nil {
		return nil
	}
	if uc.streams == nil {
		return fmt.Errorf("ingest: input includes stream samples but no StreamRepo is wired")
	}
	if err := uc.streams.WriteStream(ctx, sourceID, in.Stream); err != nil {
		return fmt.Errorf("write stream for %s: %w", sourceID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (uc *IngestActivityFromWorker) buildSource(
	activityID domain.ActivityID,
	in IngestInput,
) domain.ActivitySource {
	now := uc.now()
	src := domain.ActivitySource{
		ID:                  domain.SourceID(uc.newID()),
		ActivityID:          activityID,
		UserID:              in.UserID,
		Provider:            in.Provider,
		ExternalAccountID:   in.ExternalAccountID,
		ExternalID:          in.ExternalID,
		SourceWorkerName:    in.SourceWorkerName,
		SourceWorkerVersion: in.SourceWorkerVersion,
		SourceWorkerPackage: in.SourceWorkerPackage,
		SourceWorkerRelease: in.SourceWorkerRelease,
		RawBlobID:           in.RawBlobID,
		RawContentType:      in.RawContentType,
		RawSizeBytes:        in.RawSizeBytes,
		Parsed:              in.Payload,
		Status:              domain.SourceStatusActive,
		ReimportStatus:      domain.ReimportStatusCurrent,
		ImportedAt:          now,
		UpdatedAt:           now,
	}
	// Denormalize the start coordinate from the stream's first GPS sample so the
	// matcher can use it as a tiebreaker without reading the stream table.
	if lat, lon, ok := firstGeoPoint(in.Stream); ok {
		src.StartLat, src.StartLng = &lat, &lon
	}
	return src
}

// firstGeoPoint returns the first sample carrying both latitude and longitude.
func firstGeoPoint(samples []domain.StreamSample) (lat, lon float64, ok bool) {
	for _, s := range samples {
		if s.Latitude != nil && s.Longitude != nil {
			return *s.Latitude, *s.Longitude, true
		}
	}
	return 0, 0, false
}

// validateIngestInput rejects obviously malformed input before opening a
// transaction. These are usage errors (caller bug) rather than data
// quality issues — those are caught by Activity.Validate after merge.
func validateIngestInput(in IngestInput) error {
	if in.UserID == (domain.UserID{}) {
		return fmt.Errorf("ingest: user_id required")
	}
	if in.Provider == "" {
		return fmt.Errorf("ingest: provider required")
	}
	if in.ExternalID == "" {
		return fmt.Errorf("ingest: external_id required")
	}
	if in.SourceWorkerName == "" {
		return fmt.Errorf("ingest: source_worker_name required")
	}
	if !in.Payload.Type.Valid() {
		return fmt.Errorf("ingest: payload type %q invalid", in.Payload.Type)
	}
	if in.Payload.StartTime.IsZero() {
		return fmt.Errorf("ingest: payload start_time required")
	}
	if in.Payload.ElapsedDuration <= 0 {
		return fmt.Errorf("ingest: payload elapsed_duration must be positive")
	}
	return nil
}
