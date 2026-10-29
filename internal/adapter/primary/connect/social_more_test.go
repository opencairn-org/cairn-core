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

type fakeClubsC struct {
	port.ClubRepo
	club    domain.Club
	members map[domain.UserID]domain.ClubMemberRole
}

func (f *fakeClubsC) List(context.Context, domain.UserID, int, int) ([]domain.Club, error) {
	return []domain.Club{f.club}, nil
}
func (f *fakeClubsC) GetBySlug(_ context.Context, slug string) (domain.Club, error) {
	if slug != f.club.Slug {
		return domain.Club{}, domain.ErrNotFound
	}
	return f.club, nil
}
func (f *fakeClubsC) MemberRole(_ context.Context, _ domain.ClubID, u domain.UserID) (domain.ClubMemberRole, bool, error) {
	r, ok := f.members[u]
	return r, ok, nil
}
func (f *fakeClubsC) CountMembers(context.Context, domain.ClubID) (int, error) {
	return len(f.members), nil
}
func (f *fakeClubsC) ListMembers(_ context.Context, id domain.ClubID, _, _ int) ([]domain.ClubMember, error) {
	var out []domain.ClubMember
	for u, r := range f.members {
		out = append(out, domain.ClubMember{ClubID: id, UserID: u, Role: r})
	}
	return out, nil
}
func (f *fakeClubsC) ListClubFeed(context.Context, domain.ClubID, int, int) ([]domain.Activity, error) {
	return nil, nil
}

type fakeLinksC struct {
	port.ShareLinkRepo
	link domain.ShareLink
}

func (f *fakeLinksC) GetActive(_ context.Context, token string) (domain.ShareLink, error) {
	if token != f.link.Token {
		return domain.ShareLink{}, domain.ErrNotFound
	}
	return f.link, nil
}

type usersByNameC struct{ *fakeUsers }

func (u usersByNameC) GetUserByUsername(_ context.Context, name string) (domain.User, error) {
	for _, x := range u.users {
		if x.Username == name {
			return x, nil
		}
	}
	return domain.User{}, domain.ErrNotFound
}

func TestSocialServerReads(t *testing.T) {
	repo := inmem.NewActivityRepo()
	me, owner := domain.UserID(uuid.New()), domain.UserID(uuid.New())
	users := &fakeUsers{users: map[domain.UserID]domain.User{me: {ID: me, Username: "me"}, owner: {ID: owner, Username: "owner", ProfileIsPublic: true}}}
	pub := seedPlain(repo, owner, domain.ActivityTypeRide, actNow, domain.PrivacyPublic)
	priv := seedPlain(repo, owner, domain.ActivityTypeRide, actNow.Add(-time.Hour), domain.PrivacyPrivate)
	repo.Following[me] = map[domain.UserID]bool{owner: true}
	feeds := view.NewFeeds(repo, users, view.NewVisibility(nil, nil, nil), nil, false)
	club := domain.Club{ID: domain.ClubID(uuid.New()), Slug: "hills", Name: "Hills", IsPublic: false, Description: "climb"}
	clubs := &fakeClubsC{club: club, members: map[domain.UserID]domain.ClubMemberRole{me: domain.ClubRoleAdmin}}
	s := NewSocialServer(feeds, &fakeContent{}, nil, func(domain.Activity) string { return "e" })
	s.Clubs = view.NewClubs(clubs, users, feeds)
	s.Profiles = view.NewProfiles(usersByNameC{users}, repo, nil, nil, view.NewVisibility(nil, nil, nil))
	s.ShareLinks = &fakeLinksC{link: domain.ShareLink{Token: "tok", ActivityID: priv}}
	s.Activities = repo
	ctx := context.WithValue(t.Context(), ctxKeyUserID, me)
	anon := t.Context()

	ff, err := s.ListFollowingFeed(ctx, &v1.ListFollowingFeedRequest{})
	if err != nil || len(ff.GetItems()) != 1 || ff.GetItems()[0].GetProjected().GetId() != pub.String() {
		t.Fatalf("following %v %v", ff, err)
	}
	cl, err := s.ListClubs(ctx, &v1.ListClubsRequest{})
	if err != nil || len(cl.GetClubs()) != 1 || cl.GetClubs()[0].GetMemberCount() != 1 {
		t.Fatalf("clubs %v %v", cl, err)
	}
	gc, err := s.GetClub(ctx, &v1.GetClubRequest{Slug: "hills"})
	if err != nil || gc.GetMyRole() != "admin" || !gc.GetIsMember() || gc.GetDescription() != "climb" {
		t.Fatalf("club %v %v", gc, err)
	}
	_, err = s.GetClub(anon, &v1.GetClubRequest{Slug: "hills"})
	wantCode(t, err, connectrpc.CodePermissionDenied)
	_, err = s.GetClub(ctx, &v1.GetClubRequest{Slug: "flat"})
	wantCode(t, err, connectrpc.CodeNotFound)
	mem, err := s.ListClubMembers(ctx, &v1.ListClubMembersRequest{Slug: "hills"})
	if err != nil || len(mem.GetMembers()) != 1 || mem.GetMembers()[0].GetUser().GetUsername() != "me" {
		t.Fatalf("members %v %v", mem, err)
	}
	if _, err := s.ListClubFeed(ctx, &v1.ListClubFeedRequest{Slug: "hills"}); err != nil {
		t.Fatalf("club feed %v", err)
	}
	pr, err := s.GetProfile(anon, &v1.GetProfileRequest{Username: "owner"})
	if err != nil || !pr.GetIsPublic() || len(pr.GetActivities()) != 1 || pr.GetUser().GetUsername() != "owner" {
		t.Fatalf("profile %v %v", pr, err)
	}
	pa, err := s.GetProjectedActivity(ctx, &v1.GetProjectedActivityRequest{ActivityId: pub.String()})
	if err != nil || pa.GetIsOwner() || pa.GetOwner().GetUsername() != "owner" || pa.GetActivity().GetId() != pub.String() {
		t.Fatalf("projected %v %v", pa, err)
	}
	_, err = s.GetProjectedActivity(ctx, &v1.GetProjectedActivityRequest{ActivityId: priv.String()})
	wantCode(t, err, connectrpc.CodePermissionDenied)
	sh, err := s.GetSharedActivity(anon, &v1.GetSharedActivityRequest{Token: "tok"})
	if err != nil || sh.GetActivity().GetId() != priv.String() {
		t.Fatalf("shared %v %v", sh, err)
	}
	_, err = s.GetSharedActivity(anon, &v1.GetSharedActivityRequest{Token: "nope"})
	wantCode(t, err, connectrpc.CodeNotFound)
	if rq, err := s.ListReviewQueue(ctx, &v1.ListReviewQueueRequest{}); err != nil || rq == nil {
		t.Fatalf("review queue %v %v", rq, err)
	}
	_, err = s.ListReviewQueue(anon, &v1.ListReviewQueueRequest{})
	wantCode(t, err, connectrpc.CodeUnauthenticated)
}
