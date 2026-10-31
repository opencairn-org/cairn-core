package segmentview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

var vNow = time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)

type fakeFollows struct {
	port.FollowRepo
	following map[domain.UserID]map[domain.UserID]bool
}

func (f *fakeFollows) IsFollowing(_ context.Context, a, b domain.UserID) (bool, error) {
	return f.following[a][b], nil
}

type fakeSegBrowse struct {
	port.SegmentReader
	seg   domain.Segment
	owns  bool
	gotQ  domain.SegmentEffortQuery
	cands []domain.SegmentEffortCandidate // fastest first
}

func (f *fakeSegBrowse) GetSegment(context.Context, domain.SegmentID) (domain.Segment, error) {
	return f.seg, nil
}
func (f *fakeSegBrowse) ViewerOwnsAccount(context.Context, domain.UserID, domain.ExternalAccountID) (bool, error) {
	return f.owns, nil
}
func (f *fakeSegBrowse) ListSegmentEffortsFiltered(_ context.Context, _ domain.SegmentID, _ domain.UserID, q domain.SegmentEffortQuery) ([]domain.SegmentEffortCandidate, error) {
	f.gotQ = q
	var out []domain.SegmentEffortCandidate
	for _, c := range f.cands {
		if q.UserID == nil || *q.UserID == c.Effort.UserID {
			out = append(out, c)
		}
	}
	if q.Offset >= len(out) {
		return nil, nil
	}
	return out[q.Offset:min(len(out), q.Offset+q.Limit)], nil
}

func cand(user domain.UserID, elapsed float64, privacy domain.ActivityPrivacy) domain.SegmentEffortCandidate {
	aid := domain.ActivityID(uuid.New())
	return domain.SegmentEffortCandidate{
		Effort:   domain.SegmentEffort{ID: domain.SegmentEffortID(uuid.New()), ActivityID: aid, UserID: user, ElapsedS: elapsed, StartTime: vNow.Add(time.Duration(elapsed) * time.Second)},
		Activity: domain.Activity{ID: aid, UserID: user, Privacy: privacy},
	}
}

// openPolicies opens "segments" to the public audience for the listed owners.
type openPolicies struct {
	port.VisibilityRepo
	open map[domain.UserID]bool
}

func (p *openPolicies) GetUserPolicy(_ context.Context, id domain.UserID) (domain.VisibilityPolicy, error) {
	pol := domain.DefaultVisibilityPolicy()
	if p.open[id] {
		pol.ByAudience[domain.AudiencePublic] = domain.CategorySet{domain.CategorySummary: true, domain.CategorySegments: true}
	}
	return pol, nil
}
func (p *openPolicies) GetActivityOverride(context.Context, domain.ActivityID) (*domain.VisibilityPolicy, error) {
	return nil, nil
}

func TestSegmentsEffortsAndLeaderboardScope(t *testing.T) {
	viewer, other, friend := domain.UserID(uuid.New()), domain.UserID(uuid.New()), domain.UserID(uuid.New())
	segID := domain.SegmentID(uuid.New())
	f := &fakeSegBrowse{
		seg: domain.Segment{ID: segID, Source: domain.SegmentSourceNative, Scope: domain.SegmentScopePrivate, OwnerUserID: &viewer},
		cands: []domain.SegmentEffortCandidate{
			cand(other, 90, domain.PrivacyPrivate),     // never shown to others
			cand(friend, 95, domain.PrivacyFollowers),  // viewer follows friend
			cand(other, 100, domain.PrivacyPublic),     // other's best visible (segments opened to public)
			cand(viewer, 120, domain.PrivacyPrivate),   // own private still counts for the viewer
			cand(other, 130, domain.PrivacyPublic),     // slower, same athlete
			cand(friend, 140, domain.PrivacyFollowers), // slower, same athlete
		},
	}
	follows := &fakeFollows{following: map[domain.UserID]map[domain.UserID]bool{viewer: {friend: true}}}
	s := NewSegments(f, view.NewVisibility(nil, follows, &openPolicies{open: map[domain.UserID]bool{other: true}}))

	// Private: efforts are pinned to the viewer and the board is just them.
	if got, err := s.Efforts(t.Context(), viewer, segID, domain.SegmentEffortQuery{}); err != nil || f.gotQ.UserID == nil || *f.gotQ.UserID != viewer || len(got) != 1 {
		t.Fatalf("private efforts: %v %+v %d", err, f.gotQ, len(got))
	}
	entries, mine, err := s.Leaderboard(t.Context(), viewer, segID, domain.LeaderboardQuery{})
	if err != nil || len(entries) != 1 || entries[0].Rank != 1 || entries[0].UserID != viewer || mine == nil {
		t.Fatalf("private board: %v %+v", err, entries)
	}
	// Instance: everyone's efforts the viewer may see, ranked per athlete.
	f.seg.Scope = domain.SegmentScopeInstance
	got, err := s.Efforts(t.Context(), viewer, segID, domain.SegmentEffortQuery{Limit: 5000})
	if err != nil || f.gotQ.UserID != nil || len(got) != 5 {
		t.Fatalf("instance efforts: %v %+v %d", err, f.gotQ, len(got))
	}
	entries, mine, err = s.Leaderboard(t.Context(), viewer, segID, domain.LeaderboardQuery{})
	if err != nil || len(entries) != 3 || entries[0].UserID != friend || entries[1].UserID != other || entries[1].ElapsedS != 100 || mine == nil || mine.Rank != 3 {
		t.Fatalf("instance board: %v %+v %+v", err, entries, mine)
	}
	// A stranger sees only the public efforts opened to them.
	stranger := domain.UserID(uuid.New())
	entries, mine, err = s.Leaderboard(t.Context(), stranger, segID, domain.LeaderboardQuery{})
	if err != nil || len(entries) != 1 || entries[0].UserID != other || entries[0].Rank != 1 || mine != nil {
		t.Fatalf("stranger board: %v %+v", err, entries)
	}
	entries, _, _ = s.Leaderboard(t.Context(), viewer, segID, domain.LeaderboardQuery{Limit: 1, Offset: 1})
	if len(entries) != 1 || entries[0].UserID != other || entries[0].Rank != 2 {
		t.Fatalf("paged board: %+v", entries)
	}
	// A mirror of someone else's account is invisible.
	f.seg = domain.Segment{ID: segID, Source: domain.SegmentSourceExternal, ExternalAccountID: &[]domain.ExternalAccountID{domain.ExternalAccountID(uuid.New())}[0]}
	if _, err := s.Visible(t.Context(), viewer, segID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign mirror: %v", err)
	}
	f.owns = true
	if _, err := s.Visible(t.Context(), viewer, segID); err != nil {
		t.Fatalf("own mirror: %v", err)
	}
	if _, err := s.SearchBBox(t.Context(), viewer, port.BoundingBox{MinLat: 2, MaxLat: 1}, nil, true, 10); err == nil {
		t.Fatal("inverted bbox must fail")
	}
	_ = time.Now
}
