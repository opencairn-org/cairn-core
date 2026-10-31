// Package pipeline is the write path that keeps derived data in step with an
// activity's sources: ingest → re-cluster → follow-ups, and the follow-ups an
// edit (detach, delete, field pin, source-from, account removal) owes. Every
// primary adapter goes through it instead of chaining the stages itself.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	"github.com/opencairn-org/cairn-core/internal/usecase/besteffort"
	"github.com/opencairn-org/cairn-core/internal/usecase/match"
	"github.com/opencairn-org/cairn-core/internal/usecase/notification"
	"github.com/opencairn-org/cairn-core/internal/usecase/segment"
	"github.com/opencairn-org/cairn-core/internal/usecase/trainingload"
)

// FederationMaxAge bounds which new activities get announced, so a historical
// backfill does not flood followers. An explicit "make public" edit federates
// through the edit path without this gate.
const FederationMaxAge = 48 * time.Hour

// Training load is recomputed over a fixed window anchored at now (plus a
// warm-up), so recent CTL/ATL never reset when an activity lands.
const (
	trainingLoadDays   = 730
	trainingLoadWarmUp = 42
)

// pipelineActivityStore reads activities with their sources and saves sources.
type pipelineActivityStore interface {
	port.ActivityAggregateReader
	port.SourceWriter
}

// The stages, each satisfied by its use case (activity, match, besteffort, segment, trainingload, notification).
type (
	Ingester interface {
		Execute(ctx context.Context, in activity.IngestInput) (activity.IngestResult, error)
	}
	Reclusterer interface {
		Execute(ctx context.Context, in match.Input) (match.Result, error)
	}
	Recomputer interface {
		Execute(ctx context.Context, id domain.ActivityID) (activity.RecomputeResult, error)
	}
	BestEffortComputer interface {
		Execute(ctx context.Context, in besteffort.Input) (besteffort.Result, error)
	}
	SegmentMatcher interface {
		Execute(ctx context.Context, in segment.Input) (segment.Result, error)
	}
	TrainingLoadComputer interface {
		Execute(ctx context.Context, in trainingload.Input) (trainingload.Result, error)
	}
	PRNotifier interface {
		Execute(ctx context.Context, in notification.Input) (notification.Result, error)
	}
)

// Pipeline wires the stages. Every follow-up stage is optional (nil → skipped).
type Pipeline struct {
	Activities pipelineActivityStore
	Ingester   Ingester
	Recluster  Reclusterer // nil → no re-clustering
	Recompute  Recomputer

	Streams      port.StreamRepo
	BestEfforts  BestEffortComputer
	Segments     SegmentMatcher
	TrainingLoad TrainingLoadComputer
	PRs          PRNotifier
	Federation   port.FederationPublisher
	Events       port.ActivityEventSink

	// BorrowFrom's writes: pins, one transaction, the owner's regrouping lock.
	FieldOverrides port.FieldOverrideRepo
	Tx             port.TxManager
	Lock           port.UserLock

	// Bus carries ActivityChanged to the follow-up consumer; nil → follow-ups run inline.
	Bus             EventPublisher
	FollowUpTimeout time.Duration // 0 → defaultFollowUpTimeout

	Log *slog.Logger
	Now func() time.Time
}

func (p *Pipeline) log() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

func (p *Pipeline) now() time.Time {
	if p.Now == nil {
		return time.Now().UTC()
	}
	return p.Now()
}

// IngestSource persists a source, re-derives its grouping and dispatches the
// follow-ups for the activity it ends up in. Only the persist step can fail
// the call; re-clustering and follow-ups log and carry on (a failed
// re-cluster is repaired by the recompute_activity_merges maintenance job).
func (p *Pipeline) IngestSource(ctx context.Context, in activity.IngestInput) (activity.IngestResult, error) {
	out, err := p.Ingester.Execute(ctx, in)
	if err != nil {
		return activity.IngestResult{}, err
	}
	if p.Recluster != nil {
		if _, err := p.Recluster.Execute(ctx, match.Input{UserID: in.UserID, Around: in.Payload.StartTime}); err != nil {
			p.log().Warn("recluster failed", "user_id", in.UserID, "error", err)
		}
	}
	out = p.followSource(ctx, out)
	p.dispatch(ctx, ActivityChanged{Kind: ChangeIngested, UserID: in.UserID, ActivityID: out.ActivityID, SourceID: out.SourceID, Action: out.Action})
	return out, nil
}

// followSource follows the source into the activity re-clustering put it in:
// merging into an existing activity dissolves the new singleton, and
// everything after ingest must address the surviving one.
func (p *Pipeline) followSource(ctx context.Context, out activity.IngestResult) activity.IngestResult {
	src, err := p.Activities.GetSource(ctx, out.SourceID)
	if err != nil || src.ActivityID == out.ActivityID {
		return out
	}
	act, err := p.Activities.GetActivity(ctx, src.ActivityID)
	if err != nil {
		p.log().Warn("ingest: load re-clustered activity failed", "activity_id", src.ActivityID, "error", err)
		return out
	}
	out.ActivityID, out.Activity = act.ID, act
	if out.Action == activity.IngestActionImportedNew {
		out.Action = activity.IngestActionMergedIntoExisting // not a new activity: no created event, no federation
	}
	return out
}

// sourceFollowUps runs the per-source stages (stream aggregates, best
// efforts, segment matches) through run.
func (p *Pipeline) sourceFollowUps(ctx context.Context, sid domain.SourceID, out activity.IngestResult, run func(string, func() error)) {
	// Materialise the downsampled CAggs for this (often historical) stream —
	// the recent-window refresh policy won't, so the GPS track and charts
	// would come back empty for a backfilled activity.
	if p.Streams != nil {
		run("refresh_aggregates", func() error { return p.Streams.RefreshAggregates(ctx, sid) })
	}
	if p.BestEfforts != nil {
		run("best_efforts", func() error {
			res, err := p.BestEfforts.Execute(ctx, besteffort.Input{ActivitySourceID: sid})
			if out.SourceID == sid {
				p.publishNewRecords(out, res.NewRecords)
			}
			return err
		})
	}
	if p.Segments != nil {
		run("match_segments", func() error {
			_, err := p.Segments.Execute(ctx, segment.Input{ActivitySourceID: sid})
			return err
		})
	}
}

// Remerge re-derives an activity from its sources after an edit and dispatches
// the user-level follow-ups. A merge that leaves no source soft-deletes the
// activity, and the follow-ups retract it from remote followers.
func (p *Pipeline) Remerge(ctx context.Context, activityID domain.ActivityID) (activity.RecomputeResult, error) {
	res, err := p.Recompute.Execute(ctx, activityID)
	if err != nil {
		return res, err
	}
	p.dispatch(ctx, ActivityChanged{Kind: ChangeRemerged, UserID: res.UserID, ActivityID: activityID, SoftDeleted: res.SoftDeleted})
	return res, nil
}

// RecomputeDerived re-runs every per-source follow-up of an activity and the
// user-level ones synchronously; failures are collected, not fatal.
func (p *Pipeline) RecomputeDerived(ctx context.Context, act domain.Activity) (sources int, warnings []string, err error) {
	srcs, err := p.Activities.ListSourcesForActivity(ctx, act.ID)
	if err != nil {
		return 0, nil, fmt.Errorf("list sources: %w", err)
	}
	warn := func(stage string, err error) {
		p.log().Warn("recompute-derived step failed", "activity_id", act.ID, "step", stage, "error", err)
		warnings = append(warnings, stage+": "+err.Error())
	}
	run := func(stage string, fn func() error) {
		if err := recovered(fn); err != nil {
			warn(stage, err)
		}
	}
	for _, s := range srcs {
		p.sourceFollowUps(ctx, s.ID, activity.IngestResult{}, run)
	}
	if p.TrainingLoad != nil {
		if err := recovered(func() error { return p.trainingLoad(ctx, act.UserID) }); err != nil {
			warn("training_load", err)
		}
	}
	return len(srcs), warnings, nil
}

func (p *Pipeline) trainingLoad(ctx context.Context, userID domain.UserID) error {
	end := p.now()
	_, err := p.TrainingLoad.Execute(ctx, trainingload.Input{
		UserID: userID, Start: end.AddDate(0, 0, -trainingLoadDays), End: end, WarmUpDays: trainingLoadWarmUp,
	})
	return err
}

// publishNewRecords emits one live event per all-time record the source now holds.
func (p *Pipeline) publishNewRecords(out activity.IngestResult, records []domain.PersonalRecord) {
	if p.Events == nil {
		return
	}
	for _, r := range records {
		p.Events.Publish(domain.NewActivityEvent(domain.ActivityEventNewPR, out.Activity.UserID, "activity.new_pr").
			For(out.ActivityID, out.SourceID).
			With("activity_type", string(r.ActivityType)).With("metric", string(r.Metric)).
			With("window_kind", string(r.WindowKind)).With("window_value", strconv.Itoa(r.WindowValue)).
			With("value", strconv.FormatFloat(r.AchievedValue, 'f', 2, 64)))
	}
}

// errPanic marks a recovered panic.
var errPanic = errors.New("panic")

// recovered runs fn, turning a panic into an error (stack logged).
func recovered(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("follow-up panicked", "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("%w: %v", errPanic, r)
		}
	}()
	return fn()
}

var _ activity.Remerger = (*Pipeline)(nil)
