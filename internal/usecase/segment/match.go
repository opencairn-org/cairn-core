// Package segment holds the segment-matching use case. The orchestration
// is thin — the heavy lifting lives in domain (MatchSegment) and in
// postgres (PostGIS spatial filter). This use case glues them together
// with stream IO and persistence.
package segment

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// RankRecomputer refreshes leaderboard ranks; segmentrank.ComputeSegmentRanks satisfies it.
type RankRecomputer interface {
	ExecuteMany(ctx context.Context, segmentIDs []domain.SegmentID) (int, error)
}

// MatchSegmentsForActivity runs the segment-matching pipeline for one
// ActivitySource: read its stream → derive an expanded bounding box →
// pull candidate segments visible to the user → run domain.MatchSegment
// per candidate → write the discovered traversals as SegmentEffort rows
// → refresh the leaderboard rank columns for every touched segment.
//
// Idempotent. Reimports of the same source produce the same efforts
// (modulo algorithm-version drift); the use case wipes prior efforts
// for the source before writing the new set, all in one transaction.
type MatchSegmentsForActivity struct {
	activities port.ActivityAggregateReader
	streams    port.StreamRepo
	segments   port.SegmentEffortStore
	tx         port.TxManager

	// recomputeRanks is invoked once per segment that received a new
	// effort. nil disables the follow-up (suitable for tests).
	recomputeRanks RankRecomputer

	now   func() time.Time
	newID func() uuid.UUID

	// Bbox expansion buffer applied to the activity's stream bbox before
	// the spatial candidate query. Generous default — better to evaluate
	// a few extra segments in Go than to miss a segment whose start lies
	// just outside the strict bbox of a noisy GPS trace.
	bboxBufferMeters float64

	// Tolerance defaults. Per-segment overrides on the Segment override
	// these via EffectiveTolerances.
	tol domain.MatchTolerances

	logger *slog.Logger
}

// NewMatchSegmentsForActivity wires the use case. nil now defaults to
// time.Now (UTC); nil newID defaults to uuid.NewV7. nil logger uses
// slog.Default. tol with zero CorridorM falls back to
// DefaultMatchTolerances. recomputeRanks may be nil — when nil the
// rank-refresh follow-up is skipped (and operators must run it manually
// via /admin/segments/{id}/compute-ranks).
func NewMatchSegmentsForActivity(
	activities port.ActivityAggregateReader,
	streams port.StreamRepo,
	segments port.SegmentEffortStore,
	tx port.TxManager,
	recomputeRanks RankRecomputer,
	now func() time.Time,
	newID func() uuid.UUID,
	tol domain.MatchTolerances,
	logger *slog.Logger,
) *MatchSegmentsForActivity {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if newID == nil {
		newID = func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New()
			}
			return id
		}
	}
	if tol.CorridorM == 0 {
		tol = domain.DefaultMatchTolerances()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &MatchSegmentsForActivity{
		activities:       activities,
		streams:          streams,
		segments:         segments,
		tx:               tx,
		recomputeRanks:   recomputeRanks,
		now:              now,
		newID:            newID,
		bboxBufferMeters: 250, // ≈ a city block; conservative
		tol:              tol,
		logger:           logger,
	}
}

// Input identifies the source to match against.
type Input struct {
	ActivitySourceID domain.SourceID
}

// Result reports how the match pass concluded.
type Result struct {
	CandidatesEvaluated int
	EffortsWritten      int
	SegmentsRanked      int
	NoStream            bool
	NoGPS               bool
}

// Execute runs the matching pass.
func (uc *MatchSegmentsForActivity) Execute(ctx context.Context, in Input) (Result, error) {
	src, err := uc.activities.GetSource(ctx, in.ActivitySourceID)
	if err != nil {
		return Result{}, fmt.Errorf("load source %s: %w", in.ActivitySourceID, err)
	}
	act, err := uc.activities.GetActivity(ctx, src.ActivityID)
	if err != nil {
		return Result{}, fmt.Errorf("load activity %s: %w", src.ActivityID, err)
	}

	stream, err := uc.streams.QueryStream(ctx, domain.StreamQuery{
		ActivitySourceID: in.ActivitySourceID,
		Resolution:       domain.StreamResolutionRaw,
	})
	if err != nil {
		return Result{}, fmt.Errorf("read stream for %s: %w", in.ActivitySourceID, err)
	}
	if len(stream.Samples) == 0 {
		// Wipe any prior efforts — a reimport that lost its stream should
		// not leave dangling efforts.
		return uc.matchClearEfforts(ctx, in.ActivitySourceID, Result{NoStream: true})
	}

	minLat, maxLat, minLon, maxLon, ok := domain.SampleBoundingBox(stream.Samples)
	if !ok {
		// Stream has no GPS-bearing samples (e.g. an indoor trainer ride
		// with power+HR but no GPS). Wipe any prior efforts and return.
		return uc.matchClearEfforts(ctx, in.ActivitySourceID, Result{NoGPS: true})
	}

	minLat, maxLat, minLon, maxLon = domain.ExpandBoundingBox(minLat, maxLat, minLon, maxLon, uc.bboxBufferMeters)

	candidates, err := uc.segments.ListSegmentCandidatesForActivity(ctx, act.UserID, act.Type, port.BoundingBox{
		MinLat: minLat, MaxLat: maxLat, MinLon: minLon, MaxLon: maxLon,
	})
	if err != nil {
		return Result{}, fmt.Errorf("list segment candidates: %w", err)
	}

	// Run the in-Go matching algorithm per candidate. The set is small
	// (the bbox filter is selective on a well-tuned PostGIS GIST index)
	// and the algorithm itself is O(N·M) where N is stream length and
	// M is segment-polyline vertex count — both small.
	efforts, touchedSegments := uc.matchCandidates(act, src, candidates, stream.Samples)

	// Persist atomically.
	err = uc.tx.InTx(ctx, func(ctx context.Context) error {
		if err := uc.segments.DeleteEffortsForSource(ctx, in.ActivitySourceID); err != nil {
			return err
		}
		for _, e := range efforts {
			if err := uc.segments.SaveSegmentEffort(ctx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("persist segment efforts: %w", err)
	}

	// Refresh ranks outside the effort-write transaction.
	uc.matchRefreshRanks(ctx, in.ActivitySourceID, touchedSegments)

	return Result{
		CandidatesEvaluated: len(candidates),
		EffortsWritten:      len(efforts),
		SegmentsRanked:      len(touchedSegments),
	}, nil
}

// matchClearEfforts wipes prior efforts for the source and returns res.
func (uc *MatchSegmentsForActivity) matchClearEfforts(ctx context.Context, sourceID domain.SourceID, res Result) (Result, error) {
	if err := uc.tx.InTx(ctx, func(ctx context.Context) error {
		return uc.segments.DeleteEffortsForSource(ctx, sourceID)
	}); err != nil {
		return Result{}, fmt.Errorf("clear efforts: %w", err)
	}
	return res, nil
}

// matchCandidates runs the matcher per candidate segment.
func (uc *MatchSegmentsForActivity) matchCandidates(
	act domain.Activity,
	src domain.ActivitySource,
	candidates []domain.Segment,
	samples []domain.StreamSample,
) ([]domain.SegmentEffort, map[domain.SegmentID]struct{}) {
	var efforts []domain.SegmentEffort
	touchedSegments := make(map[domain.SegmentID]struct{})
	for _, seg := range candidates {
		polyline, err := seg.DecodedPolyline()
		if err != nil {
			uc.logger.Warn("skipping segment with undecodable polyline",
				"segment_id", seg.ID, "error", err)
			continue
		}
		effTol := seg.EffectiveTolerances(uc.tol)
		matches := domain.MatchSegment(samples, polyline, effTol)
		for _, m := range matches {
			efforts = append(efforts, uc.buildEffort(act, src, seg, samples, m))
		}
		if len(matches) > 0 {
			touchedSegments[seg.ID] = struct{}{}
		}
	}
	return efforts, touchedSegments
}

// matchRefreshRanks refreshes rank denormalizations as a separate follow-up —
// outside the effort-write transaction so a hiccup in rank recompute can't
// roll back the discovered efforts. Failures log and continue; operators can
// re-run via /admin/segments/{id}/compute-ranks.
func (uc *MatchSegmentsForActivity) matchRefreshRanks(ctx context.Context, sourceID domain.SourceID, touchedSegments map[domain.SegmentID]struct{}) {
	if uc.recomputeRanks == nil || len(touchedSegments) == 0 {
		return
	}
	ids := make([]domain.SegmentID, 0, len(touchedSegments))
	for id := range touchedSegments {
		ids = append(ids, id)
	}
	if processed, rerr := uc.recomputeRanks.ExecuteMany(ctx, ids); rerr != nil {
		uc.logger.Warn("rank refresh partially failed after match",
			"source_id", sourceID,
			"segments_processed", processed,
			"segments_total", len(ids),
			"error", rerr,
		)
	}
}

// buildEffort assembles a SegmentEffort from the activity / source / segment
// context plus the raw match result. Summary metrics over the effort
// window (avg HR, avg power, etc.) are computed in one linear pass.
func (uc *MatchSegmentsForActivity) buildEffort(
	act domain.Activity,
	src domain.ActivitySource,
	seg domain.Segment,
	samples []domain.StreamSample,
	m domain.SegmentMatch,
) domain.SegmentEffort {
	window := samples[m.StartOffset : m.EndOffset+1]
	avgHR, maxHR := avgMaxHR(window)
	avgP := avgPower(window)
	avgC := avgCadence(window)
	avgS := avgSpeed(window)

	movingS := m.ElapsedSeconds // v1: no pause detection; moving == elapsed
	return domain.SegmentEffort{
		ID:               domain.SegmentEffortID(uc.newID()),
		SegmentID:        seg.ID,
		ActivityID:       act.ID,
		ActivitySourceID: src.ID,
		UserID:           act.UserID,
		StartTime:        samples[m.StartOffset].Timestamp,
		ElapsedS:         m.ElapsedSeconds,
		MovingS:          movingS,
		StartOffset:      m.StartOffset,
		EndOffset:        m.EndOffset,
		AvgHeartRateBpm:  avgHR,
		MaxHeartRateBpm:  maxHR,
		AvgPowerW:        avgP,
		AvgCadence:       avgC,
		AvgSpeedMps:      avgS,
		CreatedAt:        uc.now(),
	}
}

// ---------------------------------------------------------------------------
// Summary helpers — single pass per channel, skipping NULL samples.
// ---------------------------------------------------------------------------

func avgMaxHR(window []domain.StreamSample) (*int, *int) {
	var (
		sum, count, max int
		seen            bool
	)
	for _, s := range window {
		if s.HeartRateBpm == nil {
			continue
		}
		v := int(*s.HeartRateBpm)
		sum += v
		count++
		if !seen || v > max {
			max = v
			seen = true
		}
	}
	if count == 0 {
		return nil, nil
	}
	avg := sum / count
	return &avg, &max
}

func avgPower(window []domain.StreamSample) *int {
	var sum, count int
	for _, s := range window {
		if s.PowerW == nil {
			continue
		}
		sum += int(*s.PowerW)
		count++
	}
	if count == 0 {
		return nil
	}
	avg := sum / count
	return &avg
}

func avgCadence(window []domain.StreamSample) *int {
	var sum, count int
	for _, s := range window {
		if s.Cadence == nil {
			continue
		}
		sum += int(*s.Cadence)
		count++
	}
	if count == 0 {
		return nil
	}
	avg := sum / count
	return &avg
}

func avgSpeed(window []domain.StreamSample) *float64 {
	var sum float64
	var count int
	for _, s := range window {
		if s.SpeedMps == nil {
			continue
		}
		sum += *s.SpeedMps
		count++
	}
	if count == 0 {
		return nil
	}
	avg := sum / float64(count)
	return &avg
}
