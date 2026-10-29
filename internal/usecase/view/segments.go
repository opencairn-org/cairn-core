package view

import (
	"context"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Segments are the caller-centric segment reads (SegmentService /
// GET /api/segments, /api/segments/{id}).
type Segments struct {
	segments port.SegmentRepo
}

// NewSegments wires the repo (nil → every read is ErrUnavailable).
func NewSegments(segments port.SegmentRepo) *Segments { return &Segments{segments: segments} }

// Segment list paging bounds.
const (
	SegmentsDefaultLimit = 50
	SegmentsMaxLimit     = 200
	segmentEffortsMax    = 1000
)

// MySegmentsPage is one page of the caller's segments.
type MySegmentsPage struct {
	// Stats is set on the first page only.
	Stats   *domain.UserSegmentStats
	Items   []domain.UserSegmentListItem
	HasMore bool
}

// Mine pages the caller's segments, most attempted first.
func (s *Segments) Mine(ctx context.Context, userID domain.UserID, activityType string, limit, offset int) (MySegmentsPage, error) {
	if s.segments == nil {
		return MySegmentsPage{}, ErrUnavailable
	}
	if limit <= 0 {
		limit = SegmentsDefaultLimit
	}
	if limit > SegmentsMaxLimit {
		limit = SegmentsMaxLimit
	}
	if offset < 0 {
		offset = 0
	}
	var page MySegmentsPage
	if offset == 0 {
		st, err := s.segments.UserSegmentStats(ctx, userID)
		if err != nil {
			return page, fmt.Errorf("segment stats: %w", err)
		}
		page.Stats = &st
	}
	// One extra row tells has_more without a count query.
	items, err := s.segments.ListUserSegmentsByEffortCount(ctx, userID, activityType, limit+1, offset)
	if err != nil {
		return page, fmt.Errorf("list segments: %w", err)
	}
	if len(items) > limit {
		page.HasMore = true
		items = items[:limit]
	}
	page.Items = items
	return page, nil
}

// Detail returns a segment with the viewer's own efforts (fastest first). A
// viewer sees a segment they have ridden or one they own; anything else is
// domain.ErrNotFound.
func (s *Segments) Detail(ctx context.Context, userID domain.UserID, id domain.SegmentID) (domain.Segment, []domain.SegmentEffort, error) {
	if s.segments == nil {
		return domain.Segment{}, nil, ErrUnavailable
	}
	seg, err := s.segments.GetSegment(ctx, id)
	if err != nil {
		return domain.Segment{}, nil, domain.ErrNotFound
	}
	efforts, err := s.segments.ListEffortsForSegment(ctx, id, userID, port.LeaderboardScopePersonalOnly, segmentEffortsMax, 0)
	if err != nil {
		return domain.Segment{}, nil, fmt.Errorf("list efforts: %w", err)
	}
	owns := seg.OwnerUserID != nil && *seg.OwnerUserID == userID
	if len(efforts) == 0 && !owns {
		return domain.Segment{}, nil, domain.ErrNotFound
	}
	return seg, efforts, nil
}

// MyEfforts pages the viewer's efforts on a visible segment, fastest first.
func (s *Segments) MyEfforts(ctx context.Context, userID domain.UserID, id domain.SegmentID, limit, offset int) ([]domain.SegmentEffort, error) {
	if _, _, err := s.Detail(ctx, userID, id); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = SegmentsDefaultLimit
	}
	if limit > SegmentsMaxLimit {
		limit = SegmentsMaxLimit
	}
	efforts, err := s.segments.ListEffortsForSegment(ctx, id, userID, port.LeaderboardScopePersonalOnly, limit, max(0, offset))
	if err != nil {
		return nil, fmt.Errorf("list efforts: %w", err)
	}
	return efforts, nil
}

// Visible loads a segment the viewer may read (owner, instance scope, or a
// mirror of one of their accounts); anything else is domain.ErrNotFound.
func (s *Segments) Visible(ctx context.Context, viewer domain.UserID, id domain.SegmentID) (domain.Segment, error) {
	if s.segments == nil {
		return domain.Segment{}, ErrUnavailable
	}
	seg, err := s.segments.GetSegment(ctx, id)
	if err != nil {
		return domain.Segment{}, domain.ErrNotFound
	}
	owns := false
	if seg.ExternalAccountID != nil {
		owns, _ = s.segments.ViewerOwnsAccount(ctx, viewer, *seg.ExternalAccountID)
	}
	if !seg.VisibleTo(viewer, owns) {
		return domain.Segment{}, domain.ErrNotFound
	}
	return seg, nil
}

// List pages the segments visible to the viewer with the filter applied.
func (s *Segments) List(ctx context.Context, viewer domain.UserID, f domain.SegmentListFilter) ([]domain.Segment, int, error) {
	if s.segments == nil {
		return nil, 0, ErrUnavailable
	}
	f.Limit = clampLimit(f.Limit)
	segs, total, err := s.segments.ListSegments(ctx, viewer, f)
	if err != nil {
		return nil, 0, fmt.Errorf("list segments: %w", err)
	}
	return segs, total, nil
}

// SearchBBox returns visible segments intersecting the box.
func (s *Segments) SearchBBox(ctx context.Context, viewer domain.UserID, box port.BoundingBox, types []domain.ActivityType, includeInstance bool, limit int) ([]domain.Segment, error) {
	if s.segments == nil {
		return nil, ErrUnavailable
	}
	if box.MinLat > box.MaxLat || box.MinLon > box.MaxLon {
		return nil, port.NewActionError(port.ActionErrorInvalid, "bounding box min must not exceed max")
	}
	segs, err := s.segments.SearchSegmentsInBBox(ctx, viewer, box, types, includeInstance, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("search segments: %w", err)
	}
	return segs, nil
}

// Efforts lists efforts on a visible segment. A private segment only shows
// the viewer's own efforts; an instance one shows everyone's.
func (s *Segments) Efforts(ctx context.Context, viewer domain.UserID, id domain.SegmentID, q domain.SegmentEffortQuery) ([]domain.SegmentEffort, error) {
	seg, err := s.Visible(ctx, viewer, id)
	if err != nil {
		return nil, err
	}
	if !seg.InstanceShared() {
		q.UserID = &viewer
	}
	q.Limit = clampLimit(q.Limit)
	efforts, err := s.segments.ListSegmentEffortsFiltered(ctx, id, q)
	if err != nil {
		return nil, fmt.Errorf("list efforts: %w", err)
	}
	return efforts, nil
}

// Leaderboard ranks athletes on an instance-scoped segment; on a private
// segment the board is just the viewer.
func (s *Segments) Leaderboard(ctx context.Context, viewer domain.UserID, id domain.SegmentID, q domain.LeaderboardQuery) ([]domain.LeaderboardEntry, *domain.LeaderboardEntry, error) {
	seg, err := s.Visible(ctx, viewer, id)
	if err != nil {
		return nil, nil, err
	}
	q.Limit = clampLimit(q.Limit)
	entries, mine, err := s.segments.SegmentLeaderboard(ctx, id, viewer, q)
	if err != nil {
		return nil, nil, fmt.Errorf("leaderboard: %w", err)
	}
	if !seg.InstanceShared() {
		entries = entries[:0]
		if mine != nil {
			mine.Rank = 1
			entries = append(entries, *mine)
		}
	}
	return entries, mine, nil
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return SegmentsDefaultLimit
	}
	return min(limit, SegmentsMaxLimit)
}
