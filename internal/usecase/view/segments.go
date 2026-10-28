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
