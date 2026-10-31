// Package segmentrank holds the leaderboard rank denormalization use
// case. Runs after every segment-effort write (typically as a follow-up
// from MatchSegmentsForActivity) so the rank columns on segment_efforts
// stay correct.
//
// The heavy lifting is a single window-function UPDATE in the adapter
// (SegmentRepo.RecomputeRanksForSegment); this package's job is to wrap
// it transactionally and to dispatch over the set of segments touched
// by one ingest.
package segmentrank

import (
	"context"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ComputeSegmentRanks recomputes the denormalized personal_rank /
// instance_rank / is_personal_record / is_instance_record columns on
// segment_efforts. Operates on one segment at a time; callers iterating
// over a set should call once per segment.
//
// Idempotent. Two concurrent invocations on the same segment may race —
// both produce the same final state (each is a full recompute), and the
// last commit wins. For a busy instance this is fine: ranks may briefly
// flicker but they always converge.
type ComputeSegmentRanks struct {
	segments port.SegmentRanker
	tx       port.TxManager
}

func NewComputeSegmentRanks(segments port.SegmentRanker, tx port.TxManager) *ComputeSegmentRanks {
	return &ComputeSegmentRanks{segments: segments, tx: tx}
}

// Input identifies one segment whose ranks need refreshing.
type Input struct {
	SegmentID domain.SegmentID
}

// Result is intentionally empty — the operation is fire-and-update.
// Callers learn whether it succeeded from the returned error alone.
type Result struct{}

func (uc *ComputeSegmentRanks) Execute(ctx context.Context, in Input) (Result, error) {
	err := uc.tx.InTx(ctx, func(ctx context.Context) error {
		return uc.segments.RecomputeRanksForSegment(ctx, in.SegmentID)
	})
	if err != nil {
		return Result{}, fmt.Errorf("recompute ranks for %s: %w", in.SegmentID, err)
	}
	return Result{}, nil
}

// ExecuteMany is a convenience wrapper for the common pattern of
// "matching just produced efforts on a handful of segments — refresh
// each one's ranks". Each segment is processed in its own transaction;
// a failure on one does not roll back the others, since rank state is
// fully recoverable by simply re-running the call.
func (uc *ComputeSegmentRanks) ExecuteMany(ctx context.Context, segmentIDs []domain.SegmentID) (int, error) {
	processed := 0
	var firstErr error
	for _, id := range segmentIDs {
		if _, err := uc.Execute(ctx, Input{SegmentID: id}); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		processed++
	}
	return processed, firstErr
}
