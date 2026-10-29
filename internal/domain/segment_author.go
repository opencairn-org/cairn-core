package domain

import (
	"errors"
	"strings"
	"time"
)

// Native segment authoring (SegmentService Create/Update/Delete and the
// REST twins). External mirrors stay read-only apart from the starred flag.

var (
	ErrSegmentNameRequired     = errors.New("segment name required")
	ErrSegmentTooFewPoints     = errors.New("segment polyline needs at least two points")
	ErrSegmentTooShort         = errors.New("segment must be at least 50 m long")
	ErrSegmentNotEditable      = errors.New("only Cairn-native segments can be edited")
	ErrSegmentPolylineLocked   = errors.New("the polyline cannot change once efforts exist; create a new segment")
	ErrSegmentKeepEfforts      = errors.New("efforts are always removed with their segment on this instance")
	ErrSegmentNotVisible       = errors.New("segment not visible to the caller")
	MinSegmentDistanceM        = 50.0
	segmentDescriptionMaxChars = 2000
)

// NativeSegmentInput is what a user supplies to create a segment.
type NativeSegmentInput struct {
	Name                 string
	Description          string
	ActivityType         ActivityType
	Polyline             string
	PolylinePrecision    int
	Scope                SegmentScope
	MatchCorridorM       *float64
	MatchStartToleranceM *float64
	MatchEndToleranceM   *float64
	Bidirectional        bool
}

// NewNativeSegment validates the input and builds the segment with its
// distance derived from the polyline.
func NewNativeSegment(id SegmentID, owner UserID, in NativeSegmentInput, now time.Time) (Segment, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Segment{}, ErrSegmentNameRequired
	}
	if in.PolylinePrecision == 0 {
		in.PolylinePrecision = 5
	}
	if in.Scope == "" {
		in.Scope = SegmentScopePrivate
	}
	points, err := DecodePolyline(in.Polyline, in.PolylinePrecision)
	if err != nil {
		return Segment{}, ErrInvalidPolyline
	}
	if len(points) < 2 {
		return Segment{}, ErrSegmentTooFewPoints
	}
	dist := PolylineDistanceM(points)
	if dist < MinSegmentDistanceM {
		return Segment{}, ErrSegmentTooShort
	}
	seg := Segment{
		ID: id, Source: SegmentSourceNative, OwnerUserID: &owner, Scope: in.Scope,
		Name: name, Description: truncate(strings.TrimSpace(in.Description), segmentDescriptionMaxChars),
		ActivityType: in.ActivityType, Polyline: in.Polyline, PolylinePrecision: in.PolylinePrecision,
		DistanceM: dist, MatchCorridorM: in.MatchCorridorM, MatchStartToleranceM: in.MatchStartToleranceM,
		MatchEndToleranceM: in.MatchEndToleranceM, Bidirectional: in.Bidirectional,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := seg.Validate(); err != nil {
		return Segment{}, err
	}
	return seg, nil
}

// PolylineDistanceM sums the great-circle length of a point sequence.
func PolylineDistanceM(points []GeoPoint) float64 {
	var d float64
	for i := 1; i < len(points); i++ {
		d += HaversineMeters(points[i-1], points[i])
	}
	return d
}

// SegmentPatch carries the editable fields; nil leaves a field as it is.
type SegmentPatch struct {
	Name                 *string
	Description          *string
	Scope                *SegmentScope
	MatchCorridorM       *float64
	MatchStartToleranceM *float64
	MatchEndToleranceM   *float64
	Bidirectional        *bool
	Starred              *bool
	Polyline             *string
	PolylinePrecision    int
}

// InstanceShared reports whether every user may read the segment.
func (s *Segment) InstanceShared() bool {
	return s.Source == SegmentSourceNative && s.Scope == SegmentScopeInstance
}

// ApplyPatch edits a native segment; hasEfforts locks the polyline. Starred
// is the only field an external mirror accepts.
func (s *Segment) ApplyPatch(p SegmentPatch, hasEfforts bool, now time.Time) error {
	if s.Source != SegmentSourceNative && p.editsNativeFields() {
		return ErrSegmentNotEditable
	}
	if err := s.applyScalars(p); err != nil {
		return err
	}
	if p.Polyline != nil {
		if hasEfforts {
			return ErrSegmentPolylineLocked
		}
		if err := s.applyPolyline(*p.Polyline, p.PolylinePrecision); err != nil {
			return err
		}
	}
	s.UpdatedAt = now
	return s.Validate()
}

// editsNativeFields reports whether the patch touches anything but the star.
func (p SegmentPatch) editsNativeFields() bool {
	return p.Name != nil || p.Description != nil || p.Scope != nil || p.Polyline != nil || p.Bidirectional != nil ||
		p.MatchCorridorM != nil || p.MatchStartToleranceM != nil || p.MatchEndToleranceM != nil
}

func (s *Segment) applyScalars(p SegmentPatch) error {
	if p.Name != nil {
		s.Name = strings.TrimSpace(*p.Name)
		if s.Name == "" {
			return ErrSegmentNameRequired
		}
	}
	if p.Description != nil {
		s.Description = truncate(strings.TrimSpace(*p.Description), segmentDescriptionMaxChars)
	}
	if p.Scope != nil {
		s.Scope = *p.Scope
	}
	if p.MatchCorridorM != nil {
		s.MatchCorridorM = p.MatchCorridorM
	}
	if p.MatchStartToleranceM != nil {
		s.MatchStartToleranceM = p.MatchStartToleranceM
	}
	if p.MatchEndToleranceM != nil {
		s.MatchEndToleranceM = p.MatchEndToleranceM
	}
	if p.Bidirectional != nil {
		s.Bidirectional = *p.Bidirectional
	}
	if p.Starred != nil {
		s.Starred = *p.Starred
	}
	return nil
}

func (s *Segment) applyPolyline(encoded string, precision int) error {
	if precision == 0 {
		precision = s.PolylinePrecision
	}
	points, err := DecodePolyline(encoded, precision)
	if err != nil {
		return ErrInvalidPolyline
	}
	if len(points) < 2 {
		return ErrSegmentTooFewPoints
	}
	s.Polyline, s.PolylinePrecision, s.DistanceM = encoded, precision, PolylineDistanceM(points)
	if s.DistanceM < MinSegmentDistanceM {
		return ErrSegmentTooShort
	}
	return nil
}

// VisibleTo reports whether viewer may read the segment: owners, every
// user for instance-scoped native segments; external mirrors are checked by
// the repository against the viewer's accounts (ownsAccount).
func (s *Segment) VisibleTo(viewer UserID, ownsAccount bool) bool {
	switch s.Source {
	case SegmentSourceNative:
		return s.Scope == SegmentScopeInstance || (s.OwnerUserID != nil && *s.OwnerUserID == viewer)
	case SegmentSourceExternal:
		return ownsAccount
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// SegmentListFilter narrows ListSegments (every field optional).
type SegmentListFilter struct {
	Sources         []SegmentSource
	Scopes          []SegmentScope
	ActivityTypes   []ActivityType
	OnlyWithEfforts bool
	OnlyStarred     bool
	Query           string
	Sort            SegmentSort
	Limit, Offset   int
}

// SegmentSort orders a segment listing.
type SegmentSort string

const (
	SegmentSortNameAsc         SegmentSort = "name_asc"
	SegmentSortDistanceDesc    SegmentSort = "distance_desc"
	SegmentSortElevationDesc   SegmentSort = "elevation_desc"
	SegmentSortEffortCountDesc SegmentSort = "effort_count_desc"
	SegmentSortCreatedAtDesc   SegmentSort = "created_at_desc"
)

// SegmentEffortQuery narrows ListSegmentEfforts.
type SegmentEffortQuery struct {
	UserID        *UserID
	From, To      time.Time // zero = unbounded; [From, To)
	FastestFirst  bool      // default: newest first
	Limit, Offset int
}

// LeaderboardEntry is one athlete's best effort on a segment.
type LeaderboardEntry struct {
	Rank        int
	UserID      UserID
	DisplayName string
	AvatarURL   string
	ElapsedS    float64
	StartTime   time.Time
	EffortID    SegmentEffortID
	ActivityID  ActivityID
}

// LeaderboardQuery narrows GetSegmentLeaderboard.
type LeaderboardQuery struct {
	From, To      time.Time
	Limit, Offset int
}
