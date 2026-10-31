package segment

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// providerEffortAttachToleranceS is how far apart (seconds) the matcher's and
// the provider's start times may lie and still describe the same traversal.
const providerEffortAttachToleranceS = 60

// providerSegmentStore upserts provider segments, writes efforts and ranks.
type providerSegmentStore interface {
	port.SegmentWriter
	port.SegmentEffortStore
	port.SegmentRanker
}

// ProviderSegments ingests provider-mirrored segments and their efforts.
// Both writes are idempotent: segments upsert by (account, external id),
// efforts by their natural key.
type ProviderSegments struct {
	Segments providerSegmentStore
	Sources  port.SourceReader
	NewID    func() uuid.UUID
}

// NewProviderSegments wires the use case.
func NewProviderSegments(segments providerSegmentStore, sources port.SourceReader) *ProviderSegments {
	return &ProviderSegments{Segments: segments, Sources: sources}
}

func (p *ProviderSegments) newID() uuid.UUID {
	if p.NewID != nil {
		return p.NewID()
	}
	return uuid.New()
}

// ProviderSegmentBatch memoises external id → SegmentID across one worker
// message, so an effort resolves a segment upserted moments earlier.
type ProviderSegmentBatch struct {
	uc    *ProviderSegments
	known map[string]domain.SegmentID
}

// Batch starts a per-message batch.
func (p *ProviderSegments) Batch() *ProviderSegmentBatch {
	return &ProviderSegmentBatch{uc: p, known: map[string]domain.SegmentID{}}
}

// Upsert find-or-creates the external segment s (ExternalID and
// ExternalAccountID set, ID ignored) and returns its id.
func (b *ProviderSegmentBatch) Upsert(ctx context.Context, s domain.Segment) (domain.SegmentID, error) {
	if s.ExternalID == nil || *s.ExternalID == "" {
		return domain.SegmentID{}, errors.New("segment event missing external_id")
	}
	if s.ExternalAccountID == nil {
		return domain.SegmentID{}, errors.New("segment event missing external_account_id")
	}
	externalID := *s.ExternalID
	segID, err := b.uc.Segments.FindSegmentIDByExternal(ctx, *s.ExternalAccountID, externalID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		s.ID = domain.SegmentID(b.uc.newID())
	case err != nil:
		return domain.SegmentID{}, fmt.Errorf("find segment %s: %w", externalID, err)
	default:
		s.ID = segID
	}
	if err := s.Validate(); err != nil {
		return domain.SegmentID{}, fmt.Errorf("segment %s: %w", externalID, err)
	}
	if err := b.uc.Segments.SaveSegment(ctx, s); err != nil {
		return domain.SegmentID{}, fmt.Errorf("save segment %s: %w", externalID, err)
	}
	b.known[externalID] = s.ID
	return s.ID, nil
}

// ProviderEffort is one provider-reported effort; Effort carries the payload
// (its ids are filled here).
type ProviderEffort struct {
	Provider           string
	AccountID          domain.ExternalAccountID
	SegmentExternalID  string
	ActivityExternalID string
	ProviderEffortID   string
	Effort             domain.SegmentEffort
}

// EffortOutcome reports what RecordEffort did with a provider effort.
type EffortOutcome int

const (
	// EffortAttached enriched the matcher's effort with the provider id.
	EffortAttached EffortOutcome = iota + 1
	// EffortDropped had no matcher effort on a GPS source; the matcher stays canonical.
	EffortDropped
	// EffortStored saved the provider effort of a streamless source.
	EffortStored
)

// ProviderEffortResult identifies the effort's segment and source for logging.
type ProviderEffortResult struct {
	Outcome   EffortOutcome
	SegmentID domain.SegmentID
	SourceID  domain.SourceID
}

// RecordEffort applies the canonical-effort rule: on a GPS-bearing source the
// geometric matcher owns the efforts and the provider record only enriches the
// overlapping one (no overlap → dropped, keeping leaderboards free of
// double-counted traversals); a streamless source stores the provider effort
// as the only signal and refreshes the segment's ranks.
func (b *ProviderSegmentBatch) RecordEffort(ctx context.Context, in ProviderEffort) (ProviderEffortResult, error) {
	segID, err := b.resolve(ctx, in.AccountID, in.SegmentExternalID)
	if err != nil {
		return ProviderEffortResult{}, err
	}
	// Efforts arrive apart from their activity, so resolve it by external id.
	src, err := b.uc.Sources.FindSourceByExternalID(ctx, in.Provider, &in.AccountID, in.ActivityExternalID)
	if err != nil {
		return ProviderEffortResult{}, fmt.Errorf("effort activity %s not found: %w", in.ActivityExternalID, err)
	}
	res := ProviderEffortResult{SegmentID: segID, SourceID: src.ID}
	e := in.Effort

	if src.Parsed.HasStream && src.StartLat != nil {
		attached, err := b.uc.Segments.AttachProviderEffortRef(ctx, segID, src.ID,
			in.ProviderEffortID, e.StartTime, providerEffortAttachToleranceS)
		if err != nil {
			return ProviderEffortResult{}, fmt.Errorf("attach provider effort %s: %w", in.ProviderEffortID, err)
		}
		res.Outcome = EffortDropped
		if attached {
			res.Outcome = EffortAttached
		}
		return res, nil
	}

	e.ID = domain.SegmentEffortID(b.uc.newID())
	e.SegmentID = segID
	e.ActivityID = src.ActivityID
	e.ActivitySourceID = src.ID
	e.UserID = src.UserID
	if err := b.uc.Segments.SaveSegmentEffort(ctx, e); err != nil {
		return ProviderEffortResult{}, fmt.Errorf("save segment effort: %w", err)
	}
	if err := b.uc.Segments.RecomputeRanksForSegment(ctx, segID); err != nil {
		return ProviderEffortResult{}, fmt.Errorf("recompute ranks for %s: %w", segID, err)
	}
	res.Outcome = EffortStored
	return res, nil
}

func (b *ProviderSegmentBatch) resolve(ctx context.Context, accountID domain.ExternalAccountID, externalID string) (domain.SegmentID, error) {
	if id, ok := b.known[externalID]; ok {
		return id, nil
	}
	id, err := b.uc.Segments.FindSegmentIDByExternal(ctx, accountID, externalID)
	if err != nil {
		return domain.SegmentID{}, fmt.Errorf("effort references unknown segment %s: %w", externalID, err)
	}
	b.known[externalID] = id
	return id, nil
}
