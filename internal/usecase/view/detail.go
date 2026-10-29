package view

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/streamalign"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Details are the per-activity detail reads of the activity page and the
// manage view, shared by REST and Connect. Optional repos may be nil; the
// corresponding read then reports ErrUnavailable.
type Details struct {
	activities port.ActivityRepo
	streams    port.StreamRepo
	segments   port.SegmentRepo
	athlete    port.AthleteProfileRepo
	engagement port.EngagementRepo
	users      port.UserRepo
	overrides  port.FieldOverrideRepo
	presence   port.WorkerPresence
	visibility *Visibility
}

// NewDetails wires the reads; every repo but activities may be nil.
func NewDetails(activities port.ActivityRepo, streams port.StreamRepo, segments port.SegmentRepo, athlete port.AthleteProfileRepo,
	engagement port.EngagementRepo, users port.UserRepo, overrides port.FieldOverrideRepo, presence port.WorkerPresence, visibility *Visibility) *Details {
	return &Details{activities: activities, streams: streams, segments: segments, athlete: athlete, engagement: engagement,
		users: users, overrides: overrides, presence: presence, visibility: visibility}
}

// ErrUnavailable: the read needs a subsystem this instance has not wired.
var ErrUnavailable = port.NewActionError(port.ActionErrorUnavailable, "not available on this instance")

// Laps returns the lap segmentation of the source with the most laps.
func (d *Details) Laps(ctx context.Context, act domain.Activity) ([]domain.ActivityLap, *domain.SourceID, error) {
	sources, err := d.activities.ListSourcesForActivity(ctx, act.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("list sources: %w", err)
	}
	laps := domain.PickLaps(sources)
	for _, s := range sources {
		if len(s.Parsed.Laps) == len(laps) && len(laps) > 0 {
			id := s.ID
			return laps, &id, nil
		}
	}
	return laps, nil, nil
}

// Zones computes time-in-zone from the primary stream and the athlete's
// thresholds as of the activity's date. An activity without a stream yields
// an empty result.
func (d *Details) Zones(ctx context.Context, act domain.Activity) (domain.ZonesResult, error) {
	if d.streams == nil {
		return domain.ZonesResult{}, ErrUnavailable
	}
	if act.PrimaryStreamSourceID == nil {
		return domain.ZonesResult{}, nil
	}
	stream, err := d.streams.QueryStream(ctx, domain.StreamQuery{
		ActivitySourceID: *act.PrimaryStreamSourceID,
		Channels:         []domain.StreamChannel{domain.StreamChannelHeartRate, domain.StreamChannelPower},
		Resolution:       domain.StreamResolutionRaw,
	})
	if err != nil {
		return domain.ZonesResult{}, fmt.Errorf("stream query: %w", err)
	}
	return domain.ComputeZones(stream.Samples, d.thresholds(ctx, act)), nil
}

func (d *Details) thresholds(ctx context.Context, act domain.Activity) domain.ThresholdAt {
	var prof *domain.AthleteProfile
	if d.athlete != nil {
		if entries, err := d.athlete.ListEntries(ctx, act.UserID); err == nil {
			prof = domain.NewAthleteProfile(entries)
		}
	}
	return func(k domain.AthleteMetricKey) (float64, bool) {
		if prof == nil {
			return 0, false
		}
		return prof.ValueAt(k, act.StartTime)
	}
}

// EffortRow is a segment effort joined to its segment and located on the
// source stream (time since stream start, cumulative distance) when the raw
// stream is available.
type EffortRow struct {
	Effort                       domain.SegmentEffort
	Segment                      domain.Segment
	StartOffsetS, EndOffsetS     *float64
	StartDistanceM, EndDistanceM *float64
}

// SegmentEfforts lists the activity's efforts with their segments.
func (d *Details) SegmentEfforts(ctx context.Context, act domain.Activity) ([]EffortRow, error) {
	if d.segments == nil {
		return nil, ErrUnavailable
	}
	efforts, err := d.segments.ListEffortsForActivity(ctx, act.ID)
	if err != nil {
		return nil, fmt.Errorf("list efforts: %w", err)
	}
	segs := map[domain.SegmentID]domain.Segment{}
	samples := map[domain.SourceID][]domain.StreamSample{}
	out := make([]EffortRow, 0, len(efforts))
	for _, e := range efforts {
		seg, ok := segs[e.SegmentID]
		if !ok {
			seg, _ = d.segments.GetSegment(ctx, e.SegmentID)
			segs[e.SegmentID] = seg
		}
		s, ok := samples[e.ActivitySourceID]
		if !ok {
			s = d.rawSamples(ctx, e.ActivitySourceID)
			samples[e.ActivitySourceID] = s
		}
		out = append(out, locateEffort(e, seg, s))
	}
	return out, nil
}

func (d *Details) rawSamples(ctx context.Context, sid domain.SourceID) []domain.StreamSample {
	if d.streams == nil {
		return nil
	}
	st, err := d.streams.QueryStream(ctx, domain.StreamQuery{ActivitySourceID: sid, Resolution: domain.StreamResolutionRaw})
	if err != nil {
		return nil
	}
	return st.Samples
}

// locateEffort maps the raw-stream sample offsets to time + distance.
func locateEffort(e domain.SegmentEffort, seg domain.Segment, s []domain.StreamSample) EffortRow {
	row := EffortRow{Effort: e, Segment: seg}
	if len(s) == 0 || e.StartOffset < 0 || e.StartOffset >= len(s) || e.EndOffset < 0 || e.EndOffset >= len(s) {
		return row
	}
	base := s[0].Timestamp
	start, end := s[e.StartOffset].Timestamp.Sub(base).Seconds(), s[e.EndOffset].Timestamp.Sub(base).Seconds()
	row.StartOffsetS, row.EndOffsetS = &start, &end
	row.StartDistanceM, row.EndDistanceM = s[e.StartOffset].DistanceM, s[e.EndOffset].DistanceM
	return row
}

// Similar groups the activity with the ones over the same route. Without
// start coordinates and a distance the group is the activity alone.
func (d *Details) Similar(ctx context.Context, act domain.Activity) (domain.SimilarGroup, error) {
	if d.segments == nil {
		return domain.SimilarGroup{}, ErrUnavailable
	}
	var matches []domain.SimilarActivity
	if act.StartLat != nil && act.StartLng != nil && act.Summary.DistanceM != nil {
		var err error
		matches, err = d.segments.FindSimilarActivities(ctx, act.ID, act.UserID, *act.StartLat, *act.StartLng, *act.Summary.DistanceM, string(act.Type))
		if err != nil {
			return domain.SimilarGroup{}, fmt.Errorf("find similar: %w", err)
		}
	}
	return domain.BuildSimilarGroup(act, matches), nil
}

// MergedStream aligns the 5 s streams of every source onto one grid. ok is
// false when no source has a stream.
func (d *Details) MergedStream(ctx context.Context, act domain.Activity) (streamalign.MergedStream, bool, error) {
	if d.streams == nil {
		return streamalign.MergedStream{}, false, ErrUnavailable
	}
	sources, err := d.activities.ListSourcesForActivity(ctx, act.ID)
	if err != nil {
		return streamalign.MergedStream{}, false, fmt.Errorf("list sources: %w", err)
	}
	streams := map[domain.SourceID]domain.Stream{}
	for _, s := range sources {
		if !s.Parsed.HasStream {
			continue
		}
		st, err := d.streams.QueryStream(ctx, domain.StreamQuery{ActivitySourceID: s.ID, Resolution: domain.StreamResolution5s})
		if err != nil || len(st.Samples) == 0 {
			continue
		}
		streams[s.ID] = st
	}
	if len(streams) == 0 {
		return streamalign.MergedStream{}, false, nil
	}
	return streamalign.Build(streams, nil, streamalign.Options{}), true, nil
}

// EngagementCard is who reacted: a local user or a federated actor.
type EngagementCard struct {
	User    domain.User // zero for remote
	Remote  bool
	Handle  string
	ActorID string
}

// KudosView is the kudos state of an activity for one viewer.
type KudosView struct {
	Count    int
	HasKudos bool
	Kudosers []EngagementCard
}

// CommentView is one comment with its author.
type CommentView struct {
	ID        string
	Body      string
	CreatedAt time.Time
	Author    EngagementCard
}

// kudoserCap bounds the people listed on a kudos read.
const kudoserCap = 50

// SocialAllowed reports whether me may see and give reactions on act: the
// owner always, others when the owner shares the "social" category with them.
func (d *Details) SocialAllowed(ctx context.Context, act domain.Activity, me domain.UserID) bool {
	if d.visibility == nil {
		return act.UserID == me
	}
	return d.visibility.For(ctx, act.UserID, &me, act.ID, false).Has(domain.CategorySocial)
}

// Kudos lists kudos, local users first then federated actors.
func (d *Details) Kudos(ctx context.Context, act domain.Activity, me domain.UserID) (KudosView, error) {
	if d.engagement == nil {
		return KudosView{}, ErrUnavailable
	}
	var v KudosView
	v.Count, _ = d.engagement.CountKudos(ctx, act.ID)
	v.HasKudos, _ = d.engagement.HasKudos(ctx, act.ID, me)
	ids, _ := d.engagement.ListKudosers(ctx, act.ID, kudoserCap)
	for _, uid := range ids {
		if u, err := d.users.GetUser(ctx, uid); err == nil {
			v.Kudosers = append(v.Kudosers, EngagementCard{User: u})
		}
	}
	remote, _ := d.engagement.CountRemoteKudos(ctx, act.ID)
	v.Count += remote
	if rks, err := d.engagement.ListRemoteKudosers(ctx, act.ID, kudoserCap); err == nil {
		for _, rk := range rks {
			v.Kudosers = append(v.Kudosers, EngagementCard{Remote: true, Handle: rk.Handle, ActorID: rk.ActorID})
		}
	}
	return v, nil
}

// commentCap bounds a comment read.
const commentCap = 200

// Comments lists comments oldest first, federated replies merged in.
func (d *Details) Comments(ctx context.Context, act domain.Activity) ([]CommentView, error) {
	if d.engagement == nil {
		return nil, ErrUnavailable
	}
	local, _ := d.engagement.ListComments(ctx, act.ID, commentCap, 0)
	out := make([]CommentView, 0, len(local))
	for _, c := range local {
		card := EngagementCard{}
		if u, err := d.users.GetUser(ctx, c.UserID); err == nil {
			card.User = u
		}
		out = append(out, CommentView{ID: c.ID.String(), Body: c.Body, CreatedAt: c.CreatedAt, Author: card})
	}
	if remote, err := d.engagement.ListRemoteComments(ctx, act.ID, commentCap); err == nil {
		for _, rc := range remote {
			out = append(out, CommentView{ID: rc.ID, Body: rc.Body, CreatedAt: rc.CreatedAt, Author: EngagementCard{Remote: true, Handle: rc.Handle, ActorID: rc.ActorID}})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Manage assembles the technical manage view of the owner's activity.
func (d *Details) Manage(ctx context.Context, act domain.Activity) (domain.ManageInfo, error) {
	sources, err := d.activities.ListAllSourcesForActivity(ctx, act.ID)
	if err != nil {
		return domain.ManageInfo{}, fmt.Errorf("list sources: %w", err)
	}
	manual := map[domain.FieldGroup]bool{}
	if d.overrides != nil {
		if pins, err := d.overrides.ListForActivity(ctx, act.ID); err == nil {
			for _, p := range pins {
				manual[p.FieldKey] = true
			}
		}
	}
	var workers []domain.LiveWorker
	if d.presence != nil {
		workers = d.presence.LiveWorkers(ctx)
	}
	return domain.BuildManageInfo(act, sources, workers, manual), nil
}

// ParsedSource returns one of the activity's sources (any status) as stored.
func (d *Details) ParsedSource(ctx context.Context, act domain.Activity, sourceID domain.SourceID) (domain.ActivitySource, error) {
	src, err := d.activities.GetSource(ctx, sourceID)
	if err != nil {
		return domain.ActivitySource{}, err
	}
	if src.ActivityID != act.ID {
		return domain.ActivitySource{}, domain.ErrNotFound
	}
	return src, nil
}
