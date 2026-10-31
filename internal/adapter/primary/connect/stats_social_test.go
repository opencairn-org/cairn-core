package connect

import (
	"context"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

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

func seedPlain(r *inmem.ActivityRepo, user domain.UserID, t domain.ActivityType, start time.Time, privacy domain.ActivityPrivacy) domain.ActivityID {
	id := domain.ActivityID(uuid.New())
	d := 12000.0
	r.Activities[id] = domain.Activity{ID: id, UserID: user, Type: t, Discipline: domain.DisciplineRideRoad, Title: "t", StartTime: start,
		ElapsedDuration: time.Hour, MovingDuration: 50 * time.Minute, Summary: domain.ActivitySummary{DistanceM: &d}, Privacy: privacy, StartPlace: "Darmstadt"}
	return id
}

func TestSearchActivities(t *testing.T) {
	f := newActFixture(t)
	f.srv.Searches = view.NewSearches(f.repo, nil)
	for i := range 3 {
		seedPlain(f.repo, f.user, domain.ActivityTypeRide, actNow.Add(-time.Duration(i)*time.Hour), domain.PrivacyPrivate)
	}
	seedPlain(f.repo, f.user, domain.ActivityTypeRun, actNow.Add(-50*time.Hour), domain.PrivacyPrivate)

	res, err := f.srv.SearchActivities(f.ctx, &v1.SearchActivitiesRequest{
		Filter: &v1.ActivityFilter{Types: []v1.ActivityType{v1.ActivityType_ACTIVITY_TYPE_RIDE}},
		Page:   &v1.PageRequest{Limit: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetTotal() != 4 || res.GetMatched() != 3 || len(res.GetActivities()) != 2 || res.GetPage().GetNextCursor() != "2" {
		t.Fatalf("total=%d matched=%d page=%d cursor=%q", res.GetTotal(), res.GetMatched(), len(res.GetActivities()), res.GetPage().GetNextCursor())
	}
	if len(res.GetFacets().GetTypes()) != 2 || len(res.GetFacets().GetYears()) != 1 {
		t.Fatalf("facets %v", res.GetFacets())
	}
	// Last page: no cursor.
	res, _ = f.srv.SearchActivities(f.ctx, &v1.SearchActivitiesRequest{Filter: &v1.ActivityFilter{Types: []v1.ActivityType{v1.ActivityType_ACTIVITY_TYPE_RIDE}}, Page: &v1.PageRequest{Limit: 2, Cursor: "2"}})
	if len(res.GetActivities()) != 1 || res.GetPage().GetNextCursor() != "" {
		t.Fatalf("last page %v", res)
	}
	_, err = f.srv.SearchActivities(f.ctx, &v1.SearchActivitiesRequest{Filter: &v1.ActivityFilter{Types: []v1.ActivityType{v1.ActivityType_ACTIVITY_TYPE_RIDE, v1.ActivityType_ACTIVITY_TYPE_RUN}}})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	_, err = f.srv.SearchActivities(f.ctx, &v1.SearchActivitiesRequest{Filter: &v1.ActivityFilter{GearIds: []string{"nope"}}})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
}

func TestActivityFilterFromProto(t *testing.T) {
	neg := -1.0
	ten := 10.0
	filter, err := activityFilterFromProto(&v1.ActivityFilter{
		Disciplines: []string{"ride_road"}, Virtual: v1.ActivityFilter_FLAG_ONLY, Race: v1.ActivityFilter_FLAG_EXCLUDE,
		DistanceMinM: &neg, DistanceMaxM: &ten,
	})
	if err != nil {
		t.Fatal(err)
	}
	if filter.Discipline != "ride_road" || filter.IsVirtual == nil || !*filter.IsVirtual || filter.IsRace == nil || *filter.IsRace || filter.IsEbike != nil {
		t.Fatalf("flags %+v", filter)
	}
	if filter.DistanceMinM != nil || filter.DistanceMaxM == nil || *filter.DistanceMaxM != 10 {
		t.Fatalf("bounds %+v", filter)
	}
	if activitySortKey(v1.ActivitySort_ACTIVITY_SORT_SPEED_DESC) != "speed_desc" || activitySortKey(v1.ActivitySort_ACTIVITY_SORT_UNSPECIFIED) != "date_desc" {
		t.Fatal("sort keys")
	}
}

func TestGetOverview(t *testing.T) {
	repo := inmem.NewActivityRepo()
	user := domain.UserID(uuid.New())
	sid := domain.SourceID(uuid.New())
	id := seedPlain(repo, user, domain.ActivityTypeRide, actNow.Add(-time.Hour), domain.PrivacyPrivate)
	a := repo.Activities[id]
	a.PrimaryStreamSourceID = &sid
	repo.Activities[id] = a
	s := NewStatsServer(view.NewOverviews(repo, func() time.Time { return actNow }), &fakeContent{}, nil, func(domain.Activity) string { return "e" })
	ctx := context.WithValue(t.Context(), ctxKeyUserID, user)
	res, err := s.GetOverview(ctx, &v1.GetOverviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetTotals().GetCount() != 1 || res.GetThisWeek().GetCount() != 1 || res.GetLastWeek().GetCount() != 0 || len(res.GetBySport()) != 1 {
		t.Fatalf("response %v", res)
	}
	if len(res.GetRecent()) != 1 || res.GetRecent()[0].GetMapImage().GetUrl() == "" || res.GetRecent()[0].GetMapImageEtag() != "e" {
		t.Fatalf("recent %v", res.GetRecent())
	}
	if res.GetThisWeekRange().GetFrom().AsTime().Weekday() != time.Monday {
		t.Fatal("week range must start on Monday")
	}
	if _, err := s.GetOverview(t.Context(), &v1.GetOverviewRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
}

func TestListHomeFeed(t *testing.T) {
	repo := inmem.NewActivityRepo()
	me, friend := domain.UserID(uuid.New()), domain.UserID(uuid.New())
	users := &fakeUsers{users: map[domain.UserID]domain.User{me: {ID: me, Username: "me"}, friend: {ID: friend, Username: "friend", DisplayName: "F"}}}
	repo.Following[me] = map[domain.UserID]bool{friend: true}
	seedPlain(repo, me, domain.ActivityTypeRide, actNow.Add(-2*time.Hour), domain.PrivacyPrivate)
	theirs := seedPlain(repo, friend, domain.ActivityTypeRun, actNow.Add(-time.Hour), domain.PrivacyPublic)
	feeds := view.NewFeeds(repo, users, view.NewVisibility(nil, nil, nil), nil, false)
	s := NewSocialServer(feeds, &fakeContent{}, nil, func(domain.Activity) string { return "e" })
	ctx := context.WithValue(t.Context(), ctxKeyUserID, me)

	res, err := s.ListHomeFeed(ctx, &v1.ListHomeFeedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetItems()) != 2 {
		t.Fatalf("items %d", len(res.GetItems()))
	}
	first := res.GetItems()[0]
	if first.GetSource() != v1.FeedItem_SOURCE_FOLLOWING || first.GetProjected().GetId() != theirs.String() || first.GetOwner().GetUsername() != "friend" {
		t.Fatalf("first %v", first)
	}
	if first.GetProjected().GetStartPlace() != "Darmstadt" && len(first.GetProjected().GetVisibleCategories()) == 0 {
		t.Fatalf("projection carries nothing: %v", first.GetProjected())
	}
	second := res.GetItems()[1]
	if second.GetSource() != v1.FeedItem_SOURCE_SELF || second.GetOwn() == nil || second.GetOwner().GetUsername() != "me" {
		t.Fatalf("second %v", second)
	}
	if res.GetPage().GetNextCursor() != "" {
		t.Fatal("short page must have no cursor")
	}
	// Paging: limit 1 yields a cursor.
	res, _ = s.ListHomeFeed(ctx, &v1.ListHomeFeedRequest{Page: &v1.PageRequest{Limit: 1}})
	if len(res.GetItems()) != 1 || res.GetPage().GetNextCursor() != "1" {
		t.Fatalf("paged %v", res)
	}
	if _, err := s.ListHomeFeed(t.Context(), &v1.ListHomeFeedRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
}

func TestProjectedAndFederatedToProto(t *testing.T) {
	lat, lng := 49.9, 8.6
	p := domain.ProjectedActivity{ID: domain.ActivityID(uuid.New()), Type: domain.ActivityTypeRun, StartTime: actNow, StartLat: &lat, StartLng: &lng,
		Visible: []domain.Category{domain.CategorySummary, domain.CategoryLocation}, MapVisible: true, PhotosVisible: true}
	out := projectedActivityToProto(p)
	if out.GetStart().GetLatitude() != 49.9 || !out.GetHasPhotos() || len(out.GetVisibleCategories()) != 2 || out.GetType() != v1.ActivityType_ACTIVITY_TYPE_RUN {
		t.Fatalf("projected %v", out)
	}
	d := 3600
	f := federatedToProto(domain.FederatedFeedItem{ActivityAPID: "https://o/a/1", Name: "n", Sport: "Run", Published: actNow, ImageURL: "https://o/i.png", DurationS: &d})
	if f.GetId() != "https://o/a/1" || f.GetMapImage().GetUrl() != "https://o/i.png" || f.GetElapsedDurationS() != 3600 {
		t.Fatalf("federated %v", f)
	}
	if c := remoteCardToProto("https://o.test/users/bob"); !c.GetRemote() || c.GetUsername() != "@bob@o.test" {
		t.Fatalf("remote card %v", c)
	}
	if userCardToProto(domain.User{}) != nil {
		t.Fatal("zero user → nil card")
	}
}
