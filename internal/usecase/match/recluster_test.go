package match

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	dmatch "github.com/opencairn-org/cairn-core/internal/domain/match"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
)

// --- in-memory fakes -------------------------------------------------------

// memRepo implements the slice of the activity port roles that ReclusterBucket +
// RecomputeActivityFromSources actually touch. Unused methods are inherited from
// the embedded interface and panic if called (which would surface a missing
// fake method rather than silently passing).
type memRepo struct {
	port.ActivityAggregateStore
	port.ActivityMatchStore
	acts       map[domain.ActivityID]domain.Activity
	srcs       map[domain.SourceID]domain.ActivitySource
	redirects  map[domain.ActivityID]domain.ActivityID
	matchState map[domain.ActivityID]string
}

func newMemRepo() *memRepo {
	return &memRepo{
		acts:       map[domain.ActivityID]domain.Activity{},
		srcs:       map[domain.SourceID]domain.ActivitySource{},
		redirects:  map[domain.ActivityID]domain.ActivityID{},
		matchState: map[domain.ActivityID]string{},
	}
}

func (m *memRepo) GetActivity(_ context.Context, id domain.ActivityID) (domain.Activity, error) {
	a, ok := m.acts[id]
	if !ok || a.DeletedAt != nil {
		return domain.Activity{}, domain.ErrNotFound
	}
	return a, nil
}

func (m *memRepo) SaveActivity(_ context.Context, a domain.Activity) error {
	m.acts[a.ID] = a
	return nil
}

func (m *memRepo) SoftDeleteActivity(_ context.Context, id domain.ActivityID, at time.Time) error {
	a := m.acts[id]
	a.ID = id
	t := at
	a.DeletedAt = &t
	m.acts[id] = a
	return nil
}

func (m *memRepo) ListSourcesForActivity(_ context.Context, id domain.ActivityID) ([]domain.ActivitySource, error) {
	var out []domain.ActivitySource
	for _, s := range m.srcs {
		if s.ActivityID == id && s.Status != domain.SourceStatusDetached {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *memRepo) ReassignSource(_ context.Context, sourceID domain.SourceID, newActivityID domain.ActivityID) error {
	s := m.srcs[sourceID]
	s.ActivityID = newActivityID
	m.srcs[sourceID] = s
	return nil
}

func (m *memRepo) SetActivityMatchState(_ context.Context, id domain.ActivityID, confidence string, needsReview bool) error {
	// match_confidence/needs_review are DB columns (set via direct UPDATE), not
	// domain.Activity fields, so just record that it was called.
	m.matchState[id] = confidence
	return nil
}

func (m *memRepo) AddActivityRedirect(_ context.Context, oldID, newID domain.ActivityID, _ string) error {
	m.redirects[oldID] = newID
	return nil
}

func (m *memRepo) ListMatchConstraints(_ context.Context, _ domain.UserID) ([]domain.MatchConstraint, error) {
	return nil, nil
}

func (m *memRepo) ListSourceRecordsInBucket(_ context.Context, userID domain.UserID, from, to time.Time) ([]domain.SourceMatchRecord, error) {
	var out []domain.SourceMatchRecord
	for _, s := range m.srcs {
		if s.UserID != userID || s.Status == domain.SourceStatusDetached {
			continue
		}
		st := s.Parsed.StartTime
		if st.Before(from) || !st.Before(to) {
			continue
		}
		out = append(out, domain.SourceMatchRecord{
			SourceID:   s.ID,
			ActivityID: s.ActivityID,
			UserID:     s.UserID,
			Provider:   s.Provider,
			ExternalID: s.ExternalID,
			SportClass: string(s.Parsed.Type),
			StartUTC:   st,
			DistanceM:  s.Parsed.Summary.DistanceM,
			MovingS:    int64(s.Parsed.MovingDuration.Seconds()),
			ElapsedS:   int64(s.Parsed.ElapsedDuration.Seconds()),
		})
	}
	return out, nil
}

// fakeDenylist denies a fixed set of (provider, externalID) pairs.
type fakeDenylist struct {
	port.SourceDenylistRepo
	denied map[string]bool // key "provider|extID"
}

func (d fakeDenylist) IsDenied(_ context.Context, _ domain.UserID, provider string, _ *domain.ExternalAccountID, externalID string) (bool, error) {
	return d.denied[provider+"|"+externalID], nil
}

// fakeSettings returns the trivial AnyProvider policy.
type fakeSettings struct{ port.UserSettingsRepo }

func (fakeSettings) GetMergePolicy(_ context.Context, _ domain.UserID, t domain.ActivityType) (domain.MergePolicy, error) {
	return domain.DefaultMergePolicyFor(t), nil
}

// fakeTx runs the function inline (no real transaction); nested InTx reuses ctx.
type fakeTx struct{}

func (fakeTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

// --- helpers ---------------------------------------------------------------

var reTime = time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

func mkSource(m *memRepo, provider string, start time.Time, distM float64) (domain.SourceID, domain.ActivityID) {
	sid := domain.SourceID(uuid.New())
	aid := domain.ActivityID(uuid.New())
	d := distM
	payload := domain.ActivitySourcePayload{
		Type:            domain.ActivityTypeRide,
		Discipline:      domain.DisciplineRideRoad,
		StartTime:       start,
		EndTime:         start.Add(time.Hour),
		ElapsedDuration: time.Hour,
		MovingDuration:  55 * time.Minute,
		Timezone:        "UTC",
		Summary:         domain.ActivitySummary{DistanceM: &d},
	}
	m.srcs[sid] = domain.ActivitySource{
		ID: sid, ActivityID: aid, UserID: testUser, Provider: provider,
		ExternalID: provider + "-" + sid.String(), Parsed: payload,
		Status: domain.SourceStatusActive, ImportedAt: reTime,
	}
	// Seed each as its own singleton activity (what ingest Execute does).
	m.acts[aid] = domain.Activity{ID: aid, UserID: testUser, Type: domain.ActivityTypeRide, StartTime: start, Privacy: domain.PrivacyPrivate}
	return sid, aid
}

var testUser = domain.UserID(uuid.New())

func newRecluster(m *memRepo) *ReclusterBucket {
	recompute := activity.NewRecomputeActivityFromSources(m, fakeSettings{}, nil, nil, nil, nil, fakeTx{}, func() time.Time { return reTime })
	n := 0
	newID := func() uuid.UUID { n++; return uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n)) }
	return NewReclusterBucket(m, recompute, nil, fakeTx{}, newID, dmatch.DefaultOptions(), nil)
}

// --- tests -----------------------------------------------------------------

// Two singleton activities for the same ride (garmin + strava, +2s) re-cluster
// into ONE: one id survives with both sources, the other is soft-deleted.
func TestReclusterBucket_MergesDuplicate(t *testing.T) {
	m := newMemRepo()
	s1, a1 := mkSource(m, "garmin", reTime, 30050)
	s2, a2 := mkSource(m, "strava", reTime.Add(2*time.Second), 30000)

	res, err := newRecluster(m).Execute(context.Background(), Input{UserID: testUser, Around: reTime})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Both sources now share one activity id.
	if m.srcs[s1].ActivityID != m.srcs[s2].ActivityID {
		t.Fatalf("sources not merged: %s vs %s", m.srcs[s1].ActivityID, m.srcs[s2].ActivityID)
	}
	survivor := m.srcs[s1].ActivityID
	if survivor != a1 && survivor != a2 {
		t.Errorf("survivor %s is neither original id", survivor)
	}
	// The other original is soft-deleted.
	dissolved := a1
	if survivor == a1 {
		dissolved = a2
	}
	if d := m.acts[dissolved]; d.DeletedAt == nil {
		t.Errorf("dissolved activity %s should be soft-deleted", dissolved)
	}
	if m.redirects[dissolved] != survivor {
		t.Errorf("redirect %s→%s missing (got %s)", dissolved, survivor, m.redirects[dissolved])
	}
	if len(res.SoftDeleted) != 1 {
		t.Errorf("want 1 soft-deleted, got %d", len(res.SoftDeleted))
	}
}

// Two genuinely different rides (3h apart) are NOT merged — both survive.
func TestReclusterBucket_KeepsDistinct(t *testing.T) {
	m := newMemRepo()
	s1, a1 := mkSource(m, "garmin", reTime, 30000)
	s2, a2 := mkSource(m, "garmin", reTime.Add(3*time.Hour), 25000)
	_ = s1
	_ = s2

	if _, err := newRecluster(m).Execute(context.Background(), Input{UserID: testUser, Around: reTime}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if m.acts[a1].DeletedAt != nil || m.acts[a2].DeletedAt != nil {
		t.Errorf("distinct activities must both survive")
	}
	if m.srcs[s1].ActivityID == m.srcs[s2].ActivityID {
		t.Errorf("distinct rides must not share an activity")
	}
}

// Same provider twice (a true conflict) must NOT merge — ≤1-per-provider rule.
func TestReclusterBucket_SameProviderConflict(t *testing.T) {
	m := newMemRepo()
	s1, a1 := mkSource(m, "garmin", reTime, 30000)
	s2, a2 := mkSource(m, "garmin", reTime.Add(2*time.Second), 30010)

	events := &captureEvents{}
	res, err := newRecluster(m).WithEvents(events).Execute(context.Background(), Input{UserID: testUser, Around: reTime})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(events.got) != 1 || events.got[0].Type != domain.AdminEventProviderConflict || events.got[0].Params["provider"] != "garmin" || *events.got[0].UserID != testUser {
		t.Errorf("conflict event: %+v", events.got)
	}
	if m.srcs[s1].ActivityID == m.srcs[s2].ActivityID {
		t.Errorf("same-provider sources must NOT merge")
	}
	if m.acts[a1].DeletedAt != nil || m.acts[a2].DeletedAt != nil {
		t.Errorf("neither activity should be dissolved on a conflict")
	}
	if res.Conflicts == 0 {
		t.Errorf("expected a recorded conflict")
	}
}

// A denylisted (detached, then re-pushed) source must NOT auto-merge even though
// it looks like a duplicate — the detach decision is durable across re-import.
func TestReclusterBucket_DenylistKeepsStandalone(t *testing.T) {
	m := newMemRepo()
	s1, a1 := mkSource(m, "garmin", reTime, 30050)
	s2, a2 := mkSource(m, "strava", reTime.Add(2*time.Second), 30000)

	// Deny the strava source's identity.
	dl := fakeDenylist{denied: map[string]bool{"strava|" + m.srcs[s2].ExternalID: true}}
	recompute := activity.NewRecomputeActivityFromSources(m, fakeSettings{}, nil, nil, nil, nil, fakeTx{}, func() time.Time { return reTime })
	n := 0
	newID := func() uuid.UUID { n++; return uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n)) }
	uc := NewReclusterBucket(m, recompute, dl, fakeTx{}, newID, dmatch.DefaultOptions(), nil)

	if _, err := uc.Execute(context.Background(), Input{UserID: testUser, Around: reTime}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if m.srcs[s1].ActivityID == m.srcs[s2].ActivityID {
		t.Errorf("denylisted source must stay standalone, but it merged")
	}
	if m.acts[a1].DeletedAt != nil || m.acts[a2].DeletedAt != nil {
		t.Errorf("neither activity should be dissolved when the dupe is denied")
	}
}

type captureEvents struct{ got []domain.AdminEvent }

func (c *captureEvents) Publish(ev domain.AdminEvent) { c.got = append(c.got, ev) }
