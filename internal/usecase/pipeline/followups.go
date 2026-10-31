package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	"github.com/opencairn-org/cairn-core/internal/usecase/notification"
)

// ActivityChangedSubject carries the follow-ups a durable write owes (stream CAIRN_EVENTS).
const ActivityChangedSubject = "cairn.events.activity.changed"

// Bounds a follow-up run; inline runs are detached from the caller, so this is their only limit.
const (
	defaultFollowUpTimeout = 10 * time.Minute
	publishTimeout         = 10 * time.Second
)

// EventPublisher is the slice of port.JobBus the pipeline publishes through.
type EventPublisher interface {
	Publish(ctx context.Context, subject, msgID string, body []byte, opts ...port.PublishOpt) error
}

// ChangeKind names the write that owes follow-ups.
type ChangeKind string

const (
	ChangeIngested ChangeKind = "ingested" // IngestSource: per-source + user stages, PRs, federation create
	ChangeRemerged ChangeKind = "remerged" // Remerge: user stages, federation delete when soft-deleted
	ChangeBorrowed ChangeKind = "borrowed" // BorrowFrom: per-source + user stages
)

// ActivityChanged is the domain event a durable write publishes; its
// follow-ups are idempotent, so redelivery converges to the same state.
type ActivityChanged struct {
	Kind        ChangeKind
	UserID      domain.UserID
	ActivityID  domain.ActivityID
	SourceID    domain.SourceID       // zero for remerged
	Action      activity.IngestAction // ingested only; merged_into_existing → no federation
	SoftDeleted bool                  // remerged only
	MsgID       string
	// Redelivery skips the non-idempotent announcements (federation) a first attempt already made.
	Redelivery bool
}

type activityChangedWire struct {
	Kind        ChangeKind            `json:"kind"`
	UserID      string                `json:"user_id"`
	ActivityID  string                `json:"activity_id"`
	SourceID    string                `json:"source_id,omitempty"`
	Action      activity.IngestAction `json:"action,omitempty"`
	SoftDeleted bool                  `json:"soft_deleted,omitempty"`
	MsgID       string                `json:"msg_id"`
}

// Marshal encodes the event as its JSON wire payload.
func (e ActivityChanged) Marshal() ([]byte, error) {
	w := activityChangedWire{
		Kind: e.Kind, UserID: e.UserID.String(), ActivityID: e.ActivityID.String(),
		Action: e.Action, SoftDeleted: e.SoftDeleted, MsgID: e.MsgID,
	}
	if e.SourceID != (domain.SourceID{}) {
		w.SourceID = e.SourceID.String()
	}
	return json.Marshal(w)
}

// DecodeActivityChanged parses a wire payload; any error is permanent.
func DecodeActivityChanged(b []byte) (ActivityChanged, error) {
	var w activityChangedWire
	if err := json.Unmarshal(b, &w); err != nil {
		return ActivityChanged{}, err
	}
	switch w.Kind {
	case ChangeIngested, ChangeRemerged, ChangeBorrowed:
	default:
		return ActivityChanged{}, fmt.Errorf("unknown kind %q", w.Kind)
	}
	uid, err := uuid.Parse(w.UserID)
	if err != nil {
		return ActivityChanged{}, fmt.Errorf("user_id: %w", err)
	}
	aid, err := uuid.Parse(w.ActivityID)
	if err != nil {
		return ActivityChanged{}, fmt.Errorf("activity_id: %w", err)
	}
	ev := ActivityChanged{
		Kind: w.Kind, UserID: domain.UserID(uid), ActivityID: domain.ActivityID(aid),
		Action: w.Action, SoftDeleted: w.SoftDeleted, MsgID: w.MsgID,
	}
	if w.Kind != ChangeRemerged {
		sid, err := uuid.Parse(w.SourceID)
		if err != nil {
			return ActivityChanged{}, fmt.Errorf("source_id: %w", err)
		}
		ev.SourceID = domain.SourceID(sid)
	}
	return ev, nil
}

func (p *Pipeline) followUpTimeout() time.Duration {
	if p.FollowUpTimeout <= 0 {
		return defaultFollowUpTimeout
	}
	return p.FollowUpTimeout
}

// dispatch hands the follow-ups to the bus, or runs them inline when none is
// wired or the publish fails. Neither path is bound to the caller's context:
// a client disconnect must not cancel follow-ups of a committed write.
func (p *Pipeline) dispatch(ctx context.Context, ev ActivityChanged) {
	ctx = context.WithoutCancel(ctx)
	ev.MsgID = fmt.Sprintf("activity-changed:%s:%s:%s", ev.Kind, ev.ActivityID, uuid.NewString())
	if p.Bus != nil {
		err := p.publish(ctx, ev)
		if err == nil {
			return
		}
		p.log().Warn("publish activity-changed failed; running follow-ups inline", "activity_id", ev.ActivityID, "kind", ev.Kind, "error", err)
	}
	_ = p.RunFollowUps(ctx, ev) // stage errors are logged; inline has no retry
}

func (p *Pipeline) publish(ctx context.Context, ev ActivityChanged) error {
	body, err := ev.Marshal()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	return p.Bus.Publish(ctx, ActivityChangedSubject, ev.MsgID, body)
}

// RunFollowUps runs every follow-up ev owes. Each stage logs and recovers;
// the joined stage errors are returned so a bus consumer retries the event
// (all stages are idempotent, federation is skipped on redelivery).
func (p *Pipeline) RunFollowUps(ctx context.Context, ev ActivityChanged) error {
	ctx, cancel := context.WithTimeout(ctx, p.followUpTimeout())
	defer cancel()
	log := p.log().With("activity_id", ev.ActivityID, "kind", ev.Kind)
	var errs []error
	run := func(stage string, fn func() error) {
		if err := recovered(fn); err != nil {
			log.Warn("follow-up failed", "stage", stage, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", stage, err))
		}
	}
	var act domain.Activity
	if ev.Kind != ChangeRemerged {
		a, err := p.Activities.GetActivity(ctx, ev.ActivityID)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			log.Info("follow-ups skipped: activity gone")
			return nil
		case err != nil:
			return fmt.Errorf("load activity: %w", err)
		}
		act = a
		out := activity.IngestResult{ActivityID: ev.ActivityID, SourceID: ev.SourceID, Activity: act, Action: ev.Action}
		p.sourceFollowUps(ctx, ev.SourceID, out, run)
	}
	if ev.Kind == ChangeRemerged && ev.SoftDeleted && p.Federation != nil && !ev.Redelivery {
		run("federation delete", func() error {
			p.Federation.PublishDelete(ctx, ev.UserID, ev.ActivityID)
			return nil
		})
	}
	if p.TrainingLoad != nil {
		run("training load", func() error { return p.trainingLoad(ctx, ev.UserID) })
	}
	if ev.Kind == ChangeIngested {
		p.ingestAnnouncements(ctx, ev, act, run)
	}
	return errors.Join(errs...)
}

// ingestAnnouncements dispatches PR notifications and, for a genuinely new
// recent activity, the federation Create.
func (p *Pipeline) ingestAnnouncements(ctx context.Context, ev ActivityChanged, act domain.Activity, run func(string, func() error)) {
	if p.PRs != nil {
		run("PR dispatch", func() error {
			_, err := p.PRs.Execute(ctx, notification.Input{ActivityID: ev.ActivityID, SourceID: ev.SourceID})
			return err
		})
	}
	if ev.Action == activity.IngestActionImportedNew && p.Federation != nil && !ev.Redelivery &&
		p.now().Sub(act.StartTime) <= FederationMaxAge {
		run("federation publish", func() error {
			p.Federation.PublishCreate(ctx, act)
			return nil
		})
	}
}
