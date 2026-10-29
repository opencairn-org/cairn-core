package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// encodeTestPolyline encodes points with precision 5 (google algorithm).
func encodeTestPolyline(points []GeoPoint) string {
	var out []byte
	var prevLat, prevLon int64
	enc := func(v int64) {
		v <<= 1
		if v < 0 {
			v = ^v
		}
		for v >= 0x20 {
			out = append(out, byte((0x20|(v&0x1f))+63))
			v >>= 5
		}
		out = append(out, byte(v+63))
	}
	for _, p := range points {
		lat, lon := int64(p.Lat*1e5+0.5), int64(p.Lon*1e5+0.5)
		enc(lat - prevLat)
		enc(lon - prevLon)
		prevLat, prevLon = lat, lon
	}
	return string(out)
}

func TestNewNativeSegmentAndPatch(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	owner := UserID(uuid.New())
	line := encodeTestPolyline([]GeoPoint{{Lat: 48.0, Lon: 11.0}, {Lat: 48.01, Lon: 11.0}}) // ~1.1 km north
	seg, err := NewNativeSegment(SegmentID(uuid.New()), owner, NativeSegmentInput{Name: " Hill ", ActivityType: ActivityTypeRide, Polyline: line}, now)
	if err != nil {
		t.Fatal(err)
	}
	if seg.Name != "Hill" || seg.Scope != SegmentScopePrivate || seg.PolylinePrecision != 5 || seg.DistanceM < 1100 || seg.DistanceM > 1130 {
		t.Fatalf("segment %+v", seg)
	}
	bad := map[string]NativeSegmentInput{
		"no name":   {ActivityType: ActivityTypeRide, Polyline: line},
		"one point": {Name: "x", ActivityType: ActivityTypeRide, Polyline: encodeTestPolyline([]GeoPoint{{Lat: 48, Lon: 11}})},
		"too short": {Name: "x", ActivityType: ActivityTypeRide, Polyline: encodeTestPolyline([]GeoPoint{{Lat: 48, Lon: 11}, {Lat: 48.0001, Lon: 11}})},
		"bad type":  {Name: "x", ActivityType: "yoga", Polyline: line},
	}
	for name, in := range bad {
		if _, err := NewNativeSegment(SegmentID(uuid.New()), owner, in, now); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	name := "Renamed"
	if err := seg.ApplyPatch(SegmentPatch{Name: &name, Polyline: &line}, true, now); !errors.Is(err, ErrSegmentPolylineLocked) {
		t.Fatalf("locked polyline: %v", err)
	}
	inst := SegmentScopeInstance
	if err := seg.ApplyPatch(SegmentPatch{Name: &name, Scope: &inst}, true, now); err != nil || seg.Name != "Renamed" || seg.Scope != inst {
		t.Fatalf("patch: %v %+v", err, seg)
	}
	ext := Segment{Source: SegmentSourceExternal, Scope: SegmentScopePrivate}
	if err := ext.ApplyPatch(SegmentPatch{Name: &name}, false, now); !errors.Is(err, ErrSegmentNotEditable) {
		t.Fatalf("external name edit: %v", err)
	}
	star := true
	acct := ExternalAccountID(uuid.New())
	id := "ext"
	ext = Segment{ID: SegmentID(uuid.New()), Source: SegmentSourceExternal, Scope: SegmentScopePrivate, ExternalID: &id, ExternalAccountID: &acct, ActivityType: ActivityTypeRide, Polyline: line, PolylinePrecision: 5, DistanceM: 1000}
	if err := ext.ApplyPatch(SegmentPatch{Starred: &star}, true, now); err != nil || !ext.Starred {
		t.Fatalf("external star: %v", err)
	}
	if !seg.VisibleTo(owner, false) || !seg.VisibleTo(UserID(uuid.New()), false) || ext.VisibleTo(owner, false) || !ext.VisibleTo(owner, true) {
		t.Fatal("visibility")
	}
}
