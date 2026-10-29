package view

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

var vNow = time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC) // a Wednesday

type fakeUsers struct {
	port.UserRepo
	users map[domain.UserID]domain.User
}

func (f *fakeUsers) GetUser(_ context.Context, id domain.UserID) (domain.User, error) {
	u, ok := f.users[id]
	if !ok {
		return domain.User{}, domain.ErrNotFound
	}
	return u, nil
}

func addActivity(r *inmem.ActivityRepo, user domain.UserID, t domain.ActivityType, start time.Time, distM float64, privacy domain.ActivityPrivacy) domain.ActivityID {
	id := domain.ActivityID(uuid.New())
	d := distM
	r.Activities[id] = domain.Activity{ID: id, UserID: user, Type: t, Discipline: domain.DisciplineRideRoad, Title: "a", StartTime: start,
		MovingDuration: time.Hour, ElapsedDuration: time.Hour, Summary: domain.ActivitySummary{DistanceM: &d}, Privacy: privacy}
	return id
}

func TestWeekBounds(t *testing.T) {
	thisStart, lastStart := WeekBounds(vNow)
	if thisStart != time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) || lastStart != time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("got %v / %v", thisStart, lastStart)
	}
	// A Monday starts its own week; a Sunday belongs to the week before.
	if s, _ := WeekBounds(time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC)); s.Day() != 26 {
		t.Fatalf("monday: %v", s)
	}
	if s, _ := WeekBounds(time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)); s.Day() != 26 {
		t.Fatalf("sunday: %v", s)
	}
}

func TestOverviewsGet(t *testing.T) {
	repo := inmem.NewActivityRepo()
	user := domain.UserID(uuid.New())
	addActivity(repo, user, domain.ActivityTypeRide, vNow.Add(-24*time.Hour), 30000, domain.PrivacyPrivate)  // this week
	addActivity(repo, user, domain.ActivityTypeRun, vNow.Add(-8*24*time.Hour), 10000, domain.PrivacyPrivate) // last week
	addActivity(repo, user, domain.ActivityTypeRun, vNow.Add(-40*24*time.Hour), 5000, domain.PrivacyPrivate)
	ov, err := NewOverviews(repo, func() time.Time { return vNow }).Get(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Totals.Count != 3 || ov.ThisWeek.Count != 1 || ov.ThisWeek.DistanceM != 30000 || ov.LastWeek.Count != 1 || ov.LastWeek.DistanceM != 10000 {
		t.Fatalf("totals %+v this %+v last %+v", ov.Totals, ov.ThisWeek, ov.LastWeek)
	}
	if len(ov.Recent) != 3 || !ov.Recent[0].StartTime.After(ov.Recent[1].StartTime) {
		t.Fatalf("recent %v", ov.Recent)
	}
	if len(ov.BySport) != 2 || ov.ThisWeekTo != ov.ThisWeekFrom.AddDate(0, 0, 7) || ov.LastWeekTo != ov.ThisWeekFrom {
		t.Fatalf("by sport %v ranges %v %v", ov.BySport, ov.ThisWeekFrom, ov.LastWeekFrom)
	}
}

func TestSearchesSearch(t *testing.T) {
	repo := inmem.NewActivityRepo()
	user := domain.UserID(uuid.New())
	for i := range 5 {
		addActivity(repo, user, domain.ActivityTypeRide, vNow.Add(-time.Duration(i)*24*time.Hour), 1000, domain.PrivacyPrivate)
	}
	addActivity(repo, user, domain.ActivityTypeRun, vNow.Add(-400*24*time.Hour), 1000, domain.PrivacyPrivate)
	uc := NewSearches(repo, nil)
	res, err := uc.Search(context.Background(), user, port.ActivityListFilter{Type: "ride", Limit: 2, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 6 || res.Matched != 5 || len(res.Activities) != 2 || res.GearPrompt {
		t.Fatalf("result total=%d matched=%d page=%d prompt=%v", res.Total, res.Matched, len(res.Activities), res.GearPrompt)
	}
	// Type facet ignores the type filter (both sports listed), years are unfiltered.
	if len(res.Types) != 2 || len(res.Years) != 2 {
		t.Fatalf("facets types=%v years=%v", res.Types, res.Years)
	}
	if res, _ := uc.Search(context.Background(), user, port.ActivityListFilter{Limit: 1000}); len(res.Activities) != 6 {
		t.Fatalf("limit cap: %d", len(res.Activities))
	}
	a := domain.Activity{}
	if GearUnset(a, false) || !GearUnset(a, true) {
		t.Fatal("GearUnset must follow the prompt flag")
	}
	a.GearSetBy = domain.GearSetByUser
	if GearUnset(a, true) {
		t.Fatal("user said no gear → not unset")
	}
}

func TestFeedsHomeMergesSources(t *testing.T) {
	repo := inmem.NewActivityRepo()
	me, friend, stranger := domain.UserID(uuid.New()), domain.UserID(uuid.New()), domain.UserID(uuid.New())
	users := &fakeUsers{users: map[domain.UserID]domain.User{
		me: {ID: me, Username: "me", DisplayName: "Me"}, friend: {ID: friend, Username: "friend", DisplayName: "Friend"},
	}}
	repo.Following[me] = map[domain.UserID]bool{friend: true}
	own := addActivity(repo, me, domain.ActivityTypeRide, vNow.Add(-2*time.Hour), 1000, domain.PrivacyPrivate)
	theirs := addActivity(repo, friend, domain.ActivityTypeRun, vNow.Add(-1*time.Hour), 1000, domain.PrivacyFollowers)
	addActivity(repo, friend, domain.ActivityTypeRun, vNow.Add(-3*time.Hour), 1000, domain.PrivacyPrivate) // private: not in the feed
	addActivity(repo, stranger, domain.ActivityTypeRun, vNow, 1000, domain.PrivacyPublic)                  // not followed

	feeds := NewFeeds(repo, users, NewVisibility(nil, nil, nil), nil, false)
	items := feeds.Home(context.Background(), me, 10, 0)
	if len(items) != 2 {
		t.Fatalf("items %d", len(items))
	}
	if items[0].Source != FeedFollowing || items[0].Projected == nil || items[0].Projected.ID != theirs || items[0].Owner.Username != "friend" {
		t.Fatalf("first %+v", items[0])
	}
	if items[1].Source != FeedSelf || items[1].Own == nil || items[1].Own.ID != own || items[1].Owner.Username != "me" {
		t.Fatalf("second %+v", items[1])
	}
	if got := feeds.Home(context.Background(), me, 1, 0); len(got) != 1 {
		t.Fatalf("limit: %d", len(got))
	}
	if RemoteHandle("https://other.test/users/alice") != "@alice@other.test" {
		t.Fatal("remote handle")
	}
}
