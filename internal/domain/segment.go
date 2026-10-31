package domain

import (
	"errors"
	"math"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Segment
//
// A Segment is a saved stretch of a route — a named climb, a sprint, a
// favourite descent. Activities are matched against the user's segments at
// import time; each match becomes a SegmentEffort with a personal-best
// rank.
//
// Two sources:
//
//	SegmentSourceExternal - mirrored from an external provider via
//	                        external_account; identity of the provider
//	                        is implicit (external_account_id → provider).
//	                        Scope-locked to PRIVATE.
//	SegmentSourceNative   - created in Cairn, can be PRIVATE or INSTANCE scope.
//
// Geometry is stored two ways for two purposes:
//   * polyline (encoded text) — wire format, fast over the network
//   * geom (PostGIS GEOGRAPHY) — set in the SQL adapter on save from the
//     decoded polyline, used for spatial indexes / candidate filtering
//
// The domain layer carries only the encoded polyline + a decoded []GeoPoint
// (computed on demand). The PostGIS columns are an adapter concern.
// ---------------------------------------------------------------------------

// SegmentSource indicates whether the segment is mirrored from an
// external provider or created natively in Cairn. Provider-agnostic by
// design — the domain doesn't care which external system the mirror
// came from; that's recovered via external_account_id lookup.
type SegmentSource string

const (
	SegmentSourceExternal SegmentSource = "external"
	SegmentSourceNative   SegmentSource = "native"
)

func (s SegmentSource) Valid() bool {
	return s == SegmentSourceExternal || s == SegmentSourceNative
}

// SegmentScope is the visibility level for a segment within the instance.
// External-mirror segments are always PRIVATE — many providers' terms of
// service limit re-sharing, so we apply the rule generically.
type SegmentScope string

const (
	SegmentScopePrivate  SegmentScope = "private"
	SegmentScopeInstance SegmentScope = "instance"
)

func (s SegmentScope) Valid() bool {
	return s == SegmentScopePrivate || s == SegmentScopeInstance
}

// ClimbCategory is the standard cycling categorisation (HC, 1..4, or none).
type ClimbCategory string

const (
	ClimbCategoryNone          ClimbCategory = "none"
	ClimbCategory4             ClimbCategory = "cat_4"
	ClimbCategory3             ClimbCategory = "cat_3"
	ClimbCategory2             ClimbCategory = "cat_2"
	ClimbCategory1             ClimbCategory = "cat_1"
	ClimbCategoryHorsCategorie ClimbCategory = "hors_categorie"
)

func (c ClimbCategory) Valid() bool {
	switch c {
	case ClimbCategoryNone, ClimbCategory4, ClimbCategory3,
		ClimbCategory2, ClimbCategory1, ClimbCategoryHorsCategorie:
		return true
	}
	return false
}

// Segment is the domain aggregate.
type Segment struct {
	ID     SegmentID
	Source SegmentSource

	// External-mirror fields. Both set together; nil for native.
	// The owning provider is recovered via external_account_id.
	ExternalID        *string
	ExternalAccountID *ExternalAccountID

	// Native field. Set when Source == SegmentSourceNative; nil for
	// external-mirror.
	OwnerUserID *UserID

	Scope        SegmentScope
	Name         string
	Description  string
	ActivityType ActivityType

	// Geometry — encoded polyline (google algorithm, precision 5 default).
	Polyline          string
	PolylinePrecision int

	// Summary metrics (computed when the segment is created or imported).
	DistanceM      float64
	ElevationGainM *float64
	ElevationLossM *float64
	MinElevationM  *float64
	MaxElevationM  *float64
	AvgGrade       *float64
	MaxGrade       *float64
	ClimbCategory  ClimbCategory

	// Matching tolerances. Nil means "use instance default" (see
	// DefaultMatchTolerances). Cairn-native segments can override per-segment.
	MatchCorridorM       *float64
	MatchStartToleranceM *float64
	MatchEndToleranceM   *float64
	Bidirectional        bool

	Starred bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate enforces the source-shape constraint that the SQL CHECK
// constraint also enforces, plus the external-segment scope rule.
func (s *Segment) Validate() error {
	if !s.Source.Valid() {
		return ErrSegmentInvalidSource
	}
	if !s.Scope.Valid() {
		return ErrSegmentInvalidScope
	}
	if !s.ActivityType.Valid() {
		return ErrSegmentInvalidActivityType
	}
	if err := s.validateSourceShape(); err != nil {
		return err
	}
	if strings.TrimSpace(s.Polyline) == "" {
		return ErrSegmentMissingPolyline
	}
	if s.PolylinePrecision != 5 && s.PolylinePrecision != 6 {
		return ErrSegmentInvalidPolylinePrecision
	}
	if s.DistanceM <= 0 {
		return ErrSegmentNonPositiveDistance
	}
	return nil
}

// validateSourceShape checks the external/native ownership rules.
func (s *Segment) validateSourceShape() error {
	switch s.Source {
	case SegmentSourceExternal:
		if s.ExternalID == nil || s.ExternalAccountID == nil {
			return ErrSegmentExternalMissingExternal
		}
		if s.OwnerUserID != nil {
			return ErrSegmentExternalUnexpectedOwner
		}
		if s.Scope != SegmentScopePrivate {
			return ErrSegmentExternalScopeNotPrivate
		}
	case SegmentSourceNative:
		if s.OwnerUserID == nil {
			return ErrSegmentNativeMissingOwner
		}
		if s.ExternalID != nil || s.ExternalAccountID != nil {
			return ErrSegmentNativeUnexpectedExternal
		}
	}
	return nil
}

// SegmentEffort is the record of one traversal of one segment within one
// activity source's stream.
type SegmentEffort struct {
	ID               SegmentEffortID
	SegmentID        SegmentID
	ActivityID       ActivityID
	ActivitySourceID SourceID
	UserID           UserID

	StartTime time.Time
	ElapsedS  float64
	MovingS   float64

	// Offsets in the source stream (zero-based sample indices into the
	// stream the source carries). The effort is the contiguous sub-range
	// stream[StartOffset:EndOffset+1].
	StartOffset int
	EndOffset   int

	// Summary metrics over the effort.
	AvgHeartRateBpm *int
	MaxHeartRateBpm *int
	AvgPowerW       *int
	AvgCadence      *int
	AvgSpeedMps     *float64

	// Ranks (denormalized; recomputed by ComputeSegmentRanks after every
	// effort write for the segment).
	PersonalRank     int
	InstanceRank     int
	IsPersonalRecord bool
	IsInstanceRecord bool

	// When the matching effort corresponds to a provider-reported effort
	// (Strava sends segment efforts alongside imports), this holds the
	// provider's effort id for cross-reference / dedup.
	ProviderEffortExternalID *string

	CreatedAt time.Time
}

// ---------------------------------------------------------------------------
// Geometry helpers
//
// Encoded-polyline decoding and great-circle distance live in the domain
// because the segment-matching algorithm (a pure use case) needs them and
// neither requires PostGIS or any other adapter dependency.
// ---------------------------------------------------------------------------

// GeoPoint is a WGS84 latitude/longitude pair in decimal degrees.
type GeoPoint struct {
	Lat float64
	Lon float64
}

// DecodedPolyline returns the segment's geometry as an in-memory slice of
// GeoPoints. Decodes lazily; callers that match many activities against
// many segments should cache the result.
func (s *Segment) DecodedPolyline() ([]GeoPoint, error) {
	return DecodePolyline(s.Polyline, s.PolylinePrecision)
}

// ErrInvalidPolyline is returned when DecodePolyline encounters malformed
// input. Callers should treat such segments as unmatchable rather than
// failing the entire match pass.
var ErrInvalidPolyline = errors.New("invalid encoded polyline")

// DecodePolyline decodes a google-format encoded polyline string into a
// slice of GeoPoints. Precision is the multiplier used at encode time
// (5 = standard, 6 = high-precision OSRM variant). Returns a non-nil
// slice (possibly empty) on success.
func DecodePolyline(encoded string, precision int) ([]GeoPoint, error) {
	if precision != 5 && precision != 6 {
		return nil, ErrInvalidPolyline
	}
	factor := math.Pow10(precision)

	var (
		points []GeoPoint
		lat    int64
		lng    int64
		index  int
		n      = len(encoded)
	)

	for index < n {
		latDelta, consumed, err := decodeSigned(encoded, index, n)
		if err != nil {
			return nil, err
		}
		index += consumed
		lat += latDelta

		lngDelta, consumed, err := decodeSigned(encoded, index, n)
		if err != nil {
			return nil, err
		}
		index += consumed
		lng += lngDelta

		points = append(points, GeoPoint{
			Lat: float64(lat) / factor,
			Lon: float64(lng) / factor,
		})
	}

	return points, nil
}

func decodeSigned(s string, start, end int) (val int64, consumed int, err error) {
	var (
		shift uint
		v     int64
	)
	for i := start; i < end; i++ {
		b := int64(s[i]) - 63
		if b < 0 || b > 63 {
			return 0, 0, ErrInvalidPolyline
		}
		v |= (b & 0x1f) << shift
		consumed++
		if b < 0x20 {
			// Unsigned -> signed (zig-zag).
			if v&1 != 0 {
				return ^(v >> 1), consumed, nil
			}
			return v >> 1, consumed, nil
		}
		shift += 5
		if shift >= 64 {
			return 0, 0, ErrInvalidPolyline
		}
	}
	return 0, 0, ErrInvalidPolyline
}

// EarthRadiusMeters — sphere approximation. The error vs WGS84 ellipsoid
// is < 0.3% for distances under ~1km, which is the regime segment-matching
// operates in. Good enough.
const EarthRadiusMeters = 6_371_000.0

// HaversineMeters returns the great-circle distance between two points.
func HaversineMeters(a, b GeoPoint) float64 {
	lat1 := a.Lat * math.Pi / 180
	lat2 := b.Lat * math.Pi / 180
	dlat := (b.Lat - a.Lat) * math.Pi / 180
	dlon := (b.Lon - a.Lon) * math.Pi / 180

	sinDLat := math.Sin(dlat / 2)
	sinDLon := math.Sin(dlon / 2)

	h := sinDLat*sinDLat + math.Cos(lat1)*math.Cos(lat2)*sinDLon*sinDLon
	c := 2 * math.Asin(math.Min(1, math.Sqrt(h)))
	return EarthRadiusMeters * c
}

// ---------------------------------------------------------------------------
// Matching defaults
// ---------------------------------------------------------------------------

// MatchTolerances bundles the three knobs the matching algorithm uses.
// Callers resolve effective tolerances per segment via the chain:
//
//	per-segment-overrides → user-settings overrides → DefaultMatchTolerances
type MatchTolerances struct {
	// CorridorM — how far off the segment's polyline a sample can stray
	// and still be considered "on" the segment.
	CorridorM float64
	// StartToleranceM — how close the first sample of a potential match
	// must be to the segment's start point.
	StartToleranceM float64
	// EndToleranceM — how close the last sample of a potential match
	// must be to the segment's end point.
	EndToleranceM float64
}

// DefaultMatchTolerances is the conservative default that catches the
// common cases (typical GPS jitter, road segments) without producing
// false positives.
//
// Tunable per segment via Segment.Match*M fields and globally via
// instance settings.
func DefaultMatchTolerances() MatchTolerances {
	return MatchTolerances{
		CorridorM:       15,
		StartToleranceM: 25,
		EndToleranceM:   25,
	}
}

// EffectiveTolerances merges a segment's per-segment overrides on top of
// the supplied base tolerances.
func (s *Segment) EffectiveTolerances(base MatchTolerances) MatchTolerances {
	out := base
	if s.MatchCorridorM != nil {
		out.CorridorM = *s.MatchCorridorM
	}
	if s.MatchStartToleranceM != nil {
		out.StartToleranceM = *s.MatchStartToleranceM
	}
	if s.MatchEndToleranceM != nil {
		out.EndToleranceM = *s.MatchEndToleranceM
	}
	return out
}

// ---------------------------------------------------------------------------
// Read-model projections for the segments landing page (#70)
// ---------------------------------------------------------------------------

// UserSegmentListItem is a lean per-segment summary for the segments landing
// page: the segment's identity plus the viewer's aggregate effort stats on it.
type UserSegmentListItem struct {
	ID             SegmentID
	Name           string
	ActivityType   string
	Source         SegmentSource
	DistanceM      float64
	ElevationGainM *float64
	AvgGrade       *float64

	// Viewer aggregates.
	EffortCount  int
	BestElapsedS float64
	LastEffortAt time.Time
	HasPR        bool // viewer holds a personal record on this segment
	HasCR        bool // viewer holds the instance (course) record
}

// UserSegmentStats is the headline status block for the segments landing page.
type UserSegmentStats struct {
	Segments int // distinct segments the viewer has efforts on
	Efforts  int
	PRs      int // efforts flagged personal record
	CRs      int // efforts flagged instance (course) record
	External int // distinct external (provider-mirrored) segments
	Native   int // distinct native (Cairn) segments
}

// Same-route heuristic: same sport, start within ~250-350 m (a lat/lng box;
// 0.0035° is ~390 m of latitude) and distance within ±15%. It clusters
// repeated loops and commutes even where segment coverage is sparse.
const (
	SimilarStartToleranceDeg = 0.0035
	SimilarDistanceTolerance = 0.15
)

// SimilarRouteQuery asks for the owner's other activities over the same route.
type SimilarRouteQuery struct {
	ActivityID        ActivityID
	UserID            UserID
	Type              ActivityType
	StartLat          float64
	StartLng          float64
	DistanceM         float64
	StartToleranceDeg float64
	DistanceTolerance float64 // fraction of DistanceM
}

// SimilarRouteQueryFor builds the same-route query for act; false when act has
// no start coordinates or distance to compare.
func SimilarRouteQueryFor(act Activity) (SimilarRouteQuery, bool) {
	if act.StartLat == nil || act.StartLng == nil || act.Summary.DistanceM == nil || *act.Summary.DistanceM <= 0 {
		return SimilarRouteQuery{}, false
	}
	return SimilarRouteQuery{
		ActivityID: act.ID, UserID: act.UserID, Type: act.Type,
		StartLat: *act.StartLat, StartLng: *act.StartLng, DistanceM: *act.Summary.DistanceM,
		StartToleranceDeg: SimilarStartToleranceDeg, DistanceTolerance: SimilarDistanceTolerance,
	}, true
}

// SimilarActivity is one activity that covers roughly the same route as a
// reference activity, detected by shared segment efforts. Carries the display
// metrics the progression view needs.
type SimilarActivity struct {
	ActivityID      ActivityID
	Title           string
	Type            string
	StartTime       time.Time
	DistanceM       *float64
	MovingS         int64
	ElapsedS        int64
	AvgHeartRateBpm *int
	AvgPowerW       *int

	SharedSegments int // segments shared with the reference activity
	TargetSegments int // total distinct segments on the reference activity
}
