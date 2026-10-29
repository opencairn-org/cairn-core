package view

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeSegBrowse struct {
	port.SegmentRepo
	seg     domain.Segment
	owns    bool
	gotQ    domain.SegmentEffortQuery
	entries []domain.LeaderboardEntry
	mine    *domain.LeaderboardEntry
}

func (f *fakeSegBrowse) GetSegment(context.Context, domain.SegmentID) (domain.Segment, error) {
	return f.seg, nil
}
func (f *fakeSegBrowse) ViewerOwnsAccount(context.Context, domain.UserID, domain.ExternalAccountID) (bool, error) {
	return f.owns, nil
}
func (f *fakeSegBrowse) ListSegmentEffortsFiltered(_ context.Context, _ domain.SegmentID, q domain.SegmentEffortQuery) ([]domain.SegmentEffort, error) {
	f.gotQ = q
	return nil, nil
}
func (f *fakeSegBrowse) SegmentLeaderboard(context.Context, domain.SegmentID, domain.UserID, domain.LeaderboardQuery) ([]domain.LeaderboardEntry, *domain.LeaderboardEntry, error) {
	return f.entries, f.mine, nil
}

func TestSegmentsEffortsAndLeaderboardScope(t *testing.T) {
	viewer, other := domain.UserID(uuid.New()), domain.UserID(uuid.New())
	segID := domain.SegmentID(uuid.New())
	f := &fakeSegBrowse{
		seg:     domain.Segment{ID: segID, Source: domain.SegmentSourceNative, Scope: domain.SegmentScopePrivate, OwnerUserID: &viewer},
		entries: []domain.LeaderboardEntry{{Rank: 1, UserID: other, ElapsedS: 100}, {Rank: 2, UserID: viewer, ElapsedS: 120}},
		mine:    &domain.LeaderboardEntry{Rank: 2, UserID: viewer, ElapsedS: 120},
	}
	s := NewSegments(f)

	// Private: efforts are pinned to the viewer and the board is just them.
	if _, err := s.Efforts(t.Context(), viewer, segID, domain.SegmentEffortQuery{}); err != nil || f.gotQ.UserID == nil || *f.gotQ.UserID != viewer || f.gotQ.Limit != SegmentsDefaultLimit {
		t.Fatalf("private efforts: %v %+v", err, f.gotQ)
	}
	entries, mine, err := s.Leaderboard(t.Context(), viewer, segID, domain.LeaderboardQuery{})
	if err != nil || len(entries) != 1 || entries[0].Rank != 1 || entries[0].UserID != viewer || mine == nil {
		t.Fatalf("private board: %v %+v", err, entries)
	}
	// Instance: everyone's efforts and the full board.
	f.seg.Scope = domain.SegmentScopeInstance
	if _, err := s.Efforts(t.Context(), other, segID, domain.SegmentEffortQuery{Limit: 5000}); err != nil || f.gotQ.UserID != nil || f.gotQ.Limit != SegmentsMaxLimit {
		t.Fatalf("instance efforts: %v %+v", err, f.gotQ)
	}
	entries, _, err = s.Leaderboard(t.Context(), other, segID, domain.LeaderboardQuery{})
	if err != nil || len(entries) != 2 {
		t.Fatalf("instance board: %v %+v", err, entries)
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
