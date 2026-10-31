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
	port.UserReader
	users map[domain.UserID]domain.User
}

func (f *fakeUsers) GetUser(_ context.Context, id domain.UserID) (domain.User, error) {
	u, ok := f.users[id]
	if !ok {
		return domain.User{}, domain.ErrNotFound
	}
	return u, nil
}

func addActivity(r *inmem.ActivityRepo, user domain.UserID, t domain.ActivityType, start time.Time, privacy domain.ActivityPrivacy) domain.ActivityID {
	id := domain.ActivityID(uuid.New())
	d := 1000.0
	r.Activities[id] = domain.Activity{ID: id, UserID: user, Type: t, Discipline: domain.DisciplineRideRoad, Title: "a", StartTime: start,
		MovingDuration: time.Hour, ElapsedDuration: time.Hour, Summary: domain.ActivitySummary{DistanceM: &d}, Privacy: privacy}
	return id
}

func TestFeedsHomeMergesSources(t *testing.T) {
	repo := inmem.NewActivityRepo()
	me, friend, stranger := domain.UserID(uuid.New()), domain.UserID(uuid.New()), domain.UserID(uuid.New())
	users := &fakeUsers{users: map[domain.UserID]domain.User{
		me: {ID: me, Username: "me", DisplayName: "Me"}, friend: {ID: friend, Username: "friend", DisplayName: "Friend"},
	}}
	repo.Following[me] = map[domain.UserID]bool{friend: true}
	own := addActivity(repo, me, domain.ActivityTypeRide, vNow.Add(-2*time.Hour), domain.PrivacyPrivate)
	theirs := addActivity(repo, friend, domain.ActivityTypeRun, vNow.Add(-1*time.Hour), domain.PrivacyFollowers)
	addActivity(repo, friend, domain.ActivityTypeRun, vNow.Add(-3*time.Hour), domain.PrivacyPrivate) // private: not in the feed
	addActivity(repo, stranger, domain.ActivityTypeRun, vNow, domain.PrivacyPublic)                  // not followed

	follows := &fakeFollows{following: repo.Following}
	feeds := NewFeeds(repo, users, NewVisibility(nil, follows, nil), nil, false)
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
