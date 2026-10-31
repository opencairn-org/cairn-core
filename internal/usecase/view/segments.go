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
	segments   port.SegmentRepo
	visibility *Visibility
}

// NewSegments wires the repo (nil → every read is ErrUnavailable). Without a
// visibility resolver, effort reads only ever show the viewer's own efforts.
func NewSegments(segments port.SegmentRepo, visibility *Visibility) *Segments {
	return &Segments{segments: segments, visibility: visibility}
}

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

// Efforts lists efforts on a visible segment that the viewer may see: their
// own, plus other athletes' whose activity shares "segments" with them. A
// private segment only shows the viewer's own efforts.
func (s *Segments) Efforts(ctx context.Context, viewer domain.UserID, id domain.SegmentID, q domain.SegmentEffortQuery) ([]domain.SegmentEffort, error) {
	seg, err := s.Visible(ctx, viewer, id)
	if err != nil {
		return nil, err
	}
	if !seg.InstanceShared() || s.visibility == nil {
		q.UserID = &viewer
	}
	want, skip := clampLimit(q.Limit), max(0, q.Offset)
	out := make([]domain.SegmentEffort, 0, want)
	err = s.scanVisible(ctx, viewer, id, q, func(c domain.SegmentEffortCandidate) bool {
		if skip > 0 {
			skip--
			return true
		}
		out = append(out, c.Effort)
		return len(out) < want
	})
	if err != nil {
		return nil, fmt.Errorf("list efforts: %w", err)
	}
	return out, nil
}

// Leaderboard ranks each athlete's fastest effort the viewer may see on an
// instance-scoped segment; on a private segment the board is just the viewer.
func (s *Segments) Leaderboard(ctx context.Context, viewer domain.UserID, id domain.SegmentID, q domain.LeaderboardQuery) ([]domain.LeaderboardEntry, *domain.LeaderboardEntry, error) {
	seg, err := s.Visible(ctx, viewer, id)
	if err != nil {
		return nil, nil, err
	}
	eq := domain.SegmentEffortQuery{From: q.From, To: q.To, FastestFirst: true}
	if !seg.InstanceShared() || s.visibility == nil {
		eq.UserID = &viewer
	}
	limit, offset := clampLimit(q.Limit), max(0, q.Offset)
	var (
		ranked []domain.LeaderboardEntry
		mine   *domain.LeaderboardEntry
		seen   = map[domain.UserID]bool{}
	)
	err = s.scanVisible(ctx, viewer, id, eq, func(c domain.SegmentEffortCandidate) bool {
		if seen[c.Effort.UserID] {
			return true
		}
		seen[c.Effort.UserID] = true
		e := domain.LeaderboardEntry{
			Rank: len(ranked) + 1, UserID: c.Effort.UserID, DisplayName: c.DisplayName, AvatarURL: c.AvatarURL,
			ElapsedS: c.Effort.ElapsedS, StartTime: c.Effort.StartTime, EffortID: c.Effort.ID, ActivityID: c.Effort.ActivityID,
		}
		if n := len(ranked); n > 0 && ranked[n-1].ElapsedS == e.ElapsedS && ranked[n-1].StartTime.Equal(e.StartTime) {
			e.Rank = ranked[n-1].Rank
		}
		ranked = append(ranked, e)
		if e.UserID == viewer {
			m := e
			mine = &m
		}
		// Ranks only depend on faster rows, so stop once the page and the
		// viewer's own row are known.
		return len(ranked) < offset+limit || mine == nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("leaderboard: %w", err)
	}
	if offset >= len(ranked) {
		return []domain.LeaderboardEntry{}, mine, nil
	}
	return ranked[offset:min(len(ranked), offset+limit)], mine, nil
}

// effortScanPage / effortScanMax bound how many effort rows one read walks.
const (
	effortScanPage = 200
	effortScanMax  = 20000
)

// scanVisible walks efforts in q's order and calls yield for each the viewer
// may see ("segments" category), until yield returns false.
func (s *Segments) scanVisible(ctx context.Context, viewer domain.UserID, id domain.SegmentID, q domain.SegmentEffortQuery, yield func(domain.SegmentEffortCandidate) bool) error {
	var chk *Checker
	if s.visibility != nil {
		chk = s.visibility.Checker(&viewer)
	}
	visible := func(c *domain.SegmentEffortCandidate) bool {
		return c.Effort.UserID == viewer || (chk != nil && chk.For(ctx, &c.Activity).Has(domain.CategorySegments))
	}
	q.Limit, q.Offset = effortScanPage, 0
	for q.Offset < effortScanMax {
		page, err := s.segments.ListSegmentEffortsFiltered(ctx, id, viewer, q)
		if err != nil {
			return err
		}
		for i := range page {
			if visible(&page[i]) && !yield(page[i]) {
				return nil
			}
		}
		if len(page) < q.Limit {
			return nil
		}
		q.Offset += q.Limit
	}
	return nil
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return SegmentsDefaultLimit
	}
	return min(limit, SegmentsMaxLimit)
}
