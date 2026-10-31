package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Borrowing errors the adapters map to 4xx.
var (
	ErrBorrowSameActivity = port.NewActionError(port.ActionErrorInvalid, "donor must differ from the target activity")
	ErrBorrowNoGroups     = port.NewActionError(port.ActionErrorInvalid, "groups must list at least one field to source")
	ErrBorrowNoRoute      = port.NewActionError(port.ActionErrorPrecondition, "donor activity has no route to borrow")
)

// borrowableGroups are the field groups a derived source may carry.
var borrowableGroups = map[domain.FieldGroup]bool{
	domain.FieldGroupGPSTrack: true, domain.FieldGroupDistance: true, domain.FieldGroupElevation: true,
	domain.FieldGroupSpeed: true, domain.FieldGroupHeartRate: true, domain.FieldGroupPower: true,
	domain.FieldGroupCadence: true, domain.FieldGroupTemperature: true, domain.FieldGroupCalories: true,
}

// BorrowInput asks to source groups of target from donor.
type BorrowInput struct {
	UserID   domain.UserID
	TargetID domain.ActivityID
	DonorID  domain.ActivityID
	Groups   []domain.FieldGroup
}

// BorrowFrom creates a derived source on the target that carries the donor's
// values for the requested groups (plus its route, rebased to the target's
// start, for gps_track) and pins those groups to it. Source, route, pins and
// re-merge are one transaction under the owner's regrouping lock, so a
// failure leaves nothing behind; the follow-ups are dispatched after it commits.
func (p *Pipeline) BorrowFrom(ctx context.Context, in BorrowInput) (domain.SourceID, error) {
	if p.FieldOverrides == nil || p.Tx == nil || p.Recompute == nil {
		return domain.SourceID{}, port.NewActionError(port.ActionErrorUnavailable, "not available")
	}
	copyStream, err := checkBorrowGroups(in)
	if err != nil {
		return domain.SourceID{}, err
	}
	src, err := p.borrowWrite(ctx, in, copyStream)
	if err != nil {
		return domain.SourceID{}, err
	}
	p.dispatch(ctx, ActivityChanged{Kind: ChangeBorrowed, UserID: in.UserID, ActivityID: in.TargetID, SourceID: src.ID})
	return src.ID, nil
}

// borrowWrite is BorrowFrom's transaction: derived source, route, pins, re-merge.
func (p *Pipeline) borrowWrite(ctx context.Context, in BorrowInput, copyStream bool) (domain.ActivitySource, error) {
	var src domain.ActivitySource
	err := p.Tx.InTx(ctx, func(ctx context.Context) error {
		if p.Lock != nil {
			if err := p.Lock.LockUser(ctx, in.UserID); err != nil {
				return err
			}
		}
		target, donor, err := p.ownedPair(ctx, in)
		if err != nil {
			return err
		}
		src = derivedSource(in, donor, copyStream, p.now())
		if err := p.Activities.SaveSource(ctx, src); err != nil {
			return fmt.Errorf("save source: %w", err)
		}
		if copyStream {
			if err := p.borrowStream(ctx, donor, src.ID, target); err != nil {
				return err
			}
		}
		return p.pinAndRecompute(ctx, in, src.ID)
	})
	return src, err
}

// pinAndRecompute pins the groups to the derived source and re-merges the target.
func (p *Pipeline) pinAndRecompute(ctx context.Context, in BorrowInput, srcID domain.SourceID) error {
	for _, g := range in.Groups {
		if err := p.FieldOverrides.Set(ctx, domain.FieldSourceOverride{ActivityID: in.TargetID, FieldKey: g, SourceID: srcID}); err != nil {
			return fmt.Errorf("pin %s: %w", g, err)
		}
	}
	_, err := p.Recompute.Execute(ctx, in.TargetID)
	return err
}

// checkBorrowGroups validates the request and reports whether the route is borrowed.
func checkBorrowGroups(in BorrowInput) (copyStream bool, err error) {
	if in.DonorID == in.TargetID {
		return false, ErrBorrowSameActivity
	}
	if len(in.Groups) == 0 {
		return false, ErrBorrowNoGroups
	}
	for _, g := range in.Groups {
		if !borrowableGroups[g] {
			return false, port.NewActionError(port.ActionErrorInvalid, fmt.Sprintf("field group %q cannot be sourced from another activity", g))
		}
		copyStream = copyStream || g == domain.FieldGroupGPSTrack
	}
	return copyStream, nil
}

// ownedPair loads the caller's live target and donor; anything else is not found.
func (p *Pipeline) ownedPair(ctx context.Context, in BorrowInput) (target, donor domain.Activity, err error) {
	load := func(id domain.ActivityID) (domain.Activity, error) {
		a, err := p.Activities.GetActivity(ctx, id)
		if err != nil || a.UserID != in.UserID || a.IsDeleted() {
			return domain.Activity{}, domain.ErrNotFound
		}
		return a, nil
	}
	if target, err = load(in.TargetID); err != nil {
		return target, donor, err
	}
	donor, err = load(in.DonorID)
	return target, donor, err
}

// derivedSource Provides exactly the requested groups, so it can never win a
// group the user did not ask for (recency would otherwise let it).
func derivedSource(in BorrowInput, donor domain.Activity, copyStream bool, now time.Time) domain.ActivitySource {
	payload := domain.ActivitySourcePayload{HasStream: copyStream}
	domain.CopySummaryGroups(&payload.Summary, donor.Summary, in.Groups)
	return domain.ActivitySource{
		ID:                  domain.SourceID(uuid.New()),
		ActivityID:          in.TargetID,
		UserID:              in.UserID,
		Provider:            "derived",
		ExternalID:          uuid.NewString(),
		SourceWorkerName:    "derived",
		SourceWorkerVersion: "1.0",
		SourceWorkerPackage: "cairn",
		Parsed:              payload,
		Status:              domain.SourceStatusActive,
		StatusReason:        "derived from activity " + in.DonorID.String(),
		ReimportStatus:      domain.ReimportStatusCurrent,
		ImportedAt:          now,
		UpdatedAt:           now,
	}
}

// borrowStream copies the donor's primary stream onto the derived source,
// shifted so its first sample aligns with the target's start.
func (p *Pipeline) borrowStream(ctx context.Context, donor domain.Activity, dst domain.SourceID, target domain.Activity) error {
	if donor.PrimaryStreamSourceID == nil || p.Streams == nil {
		return ErrBorrowNoRoute
	}
	src, err := p.Streams.QueryStream(ctx, domain.StreamQuery{ActivitySourceID: *donor.PrimaryStreamSourceID, Resolution: domain.StreamResolutionRaw})
	if err != nil {
		return fmt.Errorf("read donor stream: %w", err)
	}
	if len(src.Samples) == 0 {
		return ErrBorrowNoRoute
	}
	delta := target.StartTime.Sub(src.Samples[0].Timestamp)
	for i := range src.Samples {
		src.Samples[i].Timestamp = src.Samples[i].Timestamp.Add(delta)
	}
	if err := p.Streams.WriteStream(ctx, dst, src.Samples); err != nil {
		return fmt.Errorf("write borrowed stream: %w", err)
	}
	return nil
}
