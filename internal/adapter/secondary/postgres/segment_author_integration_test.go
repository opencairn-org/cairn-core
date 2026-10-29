//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

func TestSegmentRepoAuthoringReads(t *testing.T) {
	ctx, pool := requirePool(t)
	users := NewUserRepo(pool)
	mk := func(name string) domain.UserID {
		uid := domain.UserID(uuid.New())
		suffix := uid.String()[:8]
		if err := users.CreateUser(ctx, domain.User{
			ID: uid, Username: name + "-" + suffix, Email: name + "-" + suffix + "@example.com",
			DisplayName: name, Role: domain.UserRoleUser, Status: domain.UserStatusActive, Units: domain.UserUnitsMetric,
		}, domain.Credentials{UserID: uid}); err != nil {
			t.Fatal(err)
		}
		return uid
	}
	owner, other := mk("owner"), mk("other")
	repo := NewSegmentRepo(pool)
	now := time.Now().UTC()
	line := encodePolyline5([]domain.GeoPoint{{Lat: 48.0, Lon: 11.0}, {Lat: 48.01, Lon: 11.0}}) // ~1.1 km north of 48N 11E
	seg, err := domain.NewNativeSegment(domain.SegmentID(uuid.New()), owner, domain.NativeSegmentInput{Name: "Int Hill " + owner.String()[:8], ActivityType: domain.ActivityTypeRide, Polyline: line, Scope: domain.SegmentScopeInstance}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveSegment(ctx, seg); err != nil {
		t.Fatalf("save: %v", err)
	}
	priv, _ := domain.NewNativeSegment(domain.SegmentID(uuid.New()), owner, domain.NativeSegmentInput{Name: "Priv " + owner.String()[:8], ActivityType: domain.ActivityTypeRide, Polyline: line}, now)
	if err := repo.SaveSegment(ctx, priv); err != nil {
		t.Fatal(err)
	}

	list, total, err := repo.ListSegments(ctx, other, domain.SegmentListFilter{Query: owner.String()[:8], Sort: domain.SegmentSortCreatedAtDesc})
	if err != nil || total != 1 || len(list) != 1 || list[0].ID != seg.ID {
		t.Fatalf("other sees only the instance segment: %v total=%d %+v", err, total, list)
	}
	list, total, err = repo.ListSegments(ctx, owner, domain.SegmentListFilter{Query: owner.String()[:8], Scopes: []domain.SegmentScope{domain.SegmentScopePrivate}})
	if err != nil || total != 1 || list[0].ID != priv.ID {
		t.Fatalf("owner filters private: %v %d", err, total)
	}
	found, err := repo.SearchSegmentsInBBox(ctx, other, port.BoundingBox{MinLat: 47, MaxLat: 49, MinLon: 10, MaxLon: 12}, []domain.ActivityType{domain.ActivityTypeRide}, true, 50)
	if err != nil {
		t.Fatal(err)
	}
	hit := false
	for _, s := range found {
		if s.ID == seg.ID {
			hit = true
		}
		if s.ID == priv.ID {
			t.Fatal("private segment leaked into another user's bbox search")
		}
	}
	if !hit {
		t.Fatal("instance segment missing from bbox search")
	}
	if n, err := repo.CountEffortsForSegment(ctx, seg.ID); err != nil || n != 0 {
		t.Fatalf("count: %d %v", n, err)
	}
	entries, mine, err := repo.SegmentLeaderboard(ctx, seg.ID, owner, domain.LeaderboardQuery{})
	if err != nil || len(entries) != 0 || mine != nil {
		t.Fatalf("empty board: %v %+v %+v", err, entries, mine)
	}
	if ok, err := repo.ViewerOwnsAccount(ctx, owner, domain.ExternalAccountID(uuid.New())); err != nil || ok {
		t.Fatalf("owns unknown account: %v %v", ok, err)
	}
	if err := repo.DeleteSegment(ctx, seg.ID); err != nil {
		t.Fatal(err)
	}
	_ = repo.DeleteSegment(ctx, priv.ID)
}

func encodePolyline5(points []domain.GeoPoint) string {
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
