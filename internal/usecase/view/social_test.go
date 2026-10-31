package view

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeClubs struct {
	port.ClubRepo
	clubs   map[string]domain.Club
	members map[domain.ClubID]map[domain.UserID]domain.ClubMemberRole
	feed    []domain.Activity
}

func (f *fakeClubs) List(context.Context, domain.UserID, int, int) ([]domain.Club, error) {
	var out []domain.Club
	for _, c := range f.clubs {
		out = append(out, c)
	}
	return out, nil
}
func (f *fakeClubs) GetBySlug(_ context.Context, slug string) (domain.Club, error) {
	c, ok := f.clubs[slug]
	if !ok {
		return domain.Club{}, domain.ErrNotFound
	}
	return c, nil
}
func (f *fakeClubs) MemberRole(_ context.Context, id domain.ClubID, u domain.UserID) (domain.ClubMemberRole, bool, error) {
	r, ok := f.members[id][u]
	return r, ok, nil
}
func (f *fakeClubs) CountMembers(_ context.Context, id domain.ClubID) (int, error) {
	return len(f.members[id]), nil
}
func (f *fakeClubs) ListMembers(_ context.Context, id domain.ClubID, _, _ int) ([]domain.ClubMember, error) {
	var out []domain.ClubMember
	for u, r := range f.members[id] {
		out = append(out, domain.ClubMember{ClubID: id, UserID: u, Role: r})
	}
	return out, nil
}
func (f *fakeClubs) ListClubFeed(context.Context, domain.ClubID, int, int) ([]domain.Activity, error) {
	return f.feed, nil
}

type fakeLinks struct {
	port.ShareLinkRepo
	links map[string]domain.ShareLink
}

func (f *fakeLinks) GetActive(_ context.Context, token string) (domain.ShareLink, error) {
	l, ok := f.links[token]
	if !ok {
		return domain.ShareLink{}, domain.ErrNotFound
	}
	return l, nil
}

type fakeFollows struct {
	port.FollowRepo
	following map[domain.UserID]map[domain.UserID]bool
}

func (f *fakeFollows) IsFollowing(_ context.Context, a, b domain.UserID) (bool, error) {
	return f.following[a][b], nil
}
func (f *fakeFollows) Counts(context.Context, domain.UserID) (domain.FollowCounts, error) {
	return domain.FollowCounts{Followers: 2, Following: 1}, nil
}

func TestClubsReads(t *testing.T) {
	repo := inmem.NewActivityRepo()
	me, member, stranger := domain.UserID(uuid.New()), domain.UserID(uuid.New()), domain.UserID(uuid.New())
	users := &fakeUsers{users: map[domain.UserID]domain.User{me: {ID: me, Username: "me"}, member: {ID: member, Username: "member"}}}
	pub := domain.Club{ID: domain.ClubID(uuid.New()), Slug: "pub", Name: "Public", IsPublic: true}
	priv := domain.Club{ID: domain.ClubID(uuid.New()), Slug: "priv", Name: "Private"}
	act := addActivity(repo, member, domain.ActivityTypeRun, vNow, domain.PrivacyPublic)
	own := addActivity(repo, me, domain.ActivityTypeRun, vNow.Add(-time.Hour), domain.PrivacyPrivate)
	clubs := &fakeClubs{
		clubs:   map[string]domain.Club{"pub": pub, "priv": priv},
		members: map[domain.ClubID]map[domain.UserID]domain.ClubMemberRole{pub.ID: {member: domain.ClubRoleMember}, priv.ID: {me: domain.ClubRoleOwner}},
		feed:    []domain.Activity{repo.Activities[act], repo.Activities[own]},
	}
	feeds := NewFeeds(repo, users, NewVisibility(nil, nil, nil), nil, false)
	c := NewClubs(clubs, users, feeds)

	cards, err := c.List(context.Background(), me, 0, 0)
	if err != nil || len(cards) != 2 {
		t.Fatalf("list %v %v", cards, err)
	}
	d, err := c.Get(context.Background(), "priv", &me)
	if err != nil || !d.IsMember || d.Role != domain.ClubRoleOwner || d.Members != 1 {
		t.Fatalf("own private %+v %v", d, err)
	}
	if _, err := c.Get(context.Background(), "priv", &stranger); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger on private: %v", err)
	}
	if _, err := c.Get(context.Background(), "nope", nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if d, err := c.Get(context.Background(), "pub", nil); err != nil || d.IsMember {
		t.Fatalf("anonymous on public %+v %v", d, err)
	}
	rows, err := c.Members(context.Background(), "pub", nil, 0, 0)
	if err != nil || len(rows) != 1 || rows[0].User.Username != "member" {
		t.Fatalf("members %v %v", rows, err)
	}
	items, err := c.Feed(context.Background(), "pub", &me, 0, 0)
	if err != nil || len(items) != 2 {
		t.Fatalf("feed %v %v", items, err)
	}
	if items[0].Projected == nil || items[0].Owner.Username != "member" || items[1].Own == nil || items[1].Source != FeedSelf {
		t.Fatalf("feed rows %+v %+v", items[0], items[1])
	}
}

func TestProjectedAndShared(t *testing.T) {
	repo := inmem.NewActivityRepo()
	owner, viewer := domain.UserID(uuid.New()), domain.UserID(uuid.New())
	users := &fakeUsers{users: map[domain.UserID]domain.User{owner: {ID: owner, Username: "owner"}}}
	public := addActivity(repo, owner, domain.ActivityTypeRide, vNow, domain.PrivacyPublic)
	private := addActivity(repo, owner, domain.ActivityTypeRide, vNow, domain.PrivacyPrivate)
	feeds := NewFeeds(repo, users, NewVisibility(nil, nil, nil), nil, false)

	p, u, isOwner, err := feeds.Projected(context.Background(), public, &viewer, false)
	if err != nil || isOwner || u.Username != "owner" || p.ID != public {
		t.Fatalf("projected %+v %v %v", p, isOwner, err)
	}
	if _, _, isOwner, err := feeds.Projected(context.Background(), private, &owner, false); err != nil || !isOwner {
		t.Fatalf("owner on private: %v %v", isOwner, err)
	}
	if _, _, _, err := feeds.Projected(context.Background(), private, &viewer, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger on private: %v", err)
	}
	if _, _, _, err := feeds.Projected(context.Background(), domain.ActivityID(uuid.New()), nil, false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	links := &fakeLinks{links: map[string]domain.ShareLink{"tok": {Token: "tok", ActivityID: private}}}
	if p, u, err := feeds.Shared(context.Background(), links, "tok", nil); err != nil || p.ID != private || u.Username != "owner" {
		t.Fatalf("shared %+v %v %v", p, u, err)
	}
	if _, _, err := feeds.Shared(context.Background(), links, "nope", nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("bad token: %v", err)
	}
	if _, _, err := feeds.Shared(context.Background(), nil, "tok", nil); err == nil {
		t.Fatal("no link repo must be unavailable")
	}
}

func TestProfilesGet(t *testing.T) {
	repo := inmem.NewActivityRepo()
	owner, follower, stranger := domain.UserID(uuid.New()), domain.UserID(uuid.New()), domain.UserID(uuid.New())
	users := &fakeUsers{users: map[domain.UserID]domain.User{owner: {ID: owner, Username: "owner"}}}
	addActivity(repo, owner, domain.ActivityTypeRun, vNow, domain.PrivacyPublic)
	addActivity(repo, owner, domain.ActivityTypeRun, vNow.Add(-time.Hour), domain.PrivacyPrivate)
	follows := &fakeFollows{following: map[domain.UserID]map[domain.UserID]bool{follower: {owner: true}}}
	p := NewProfiles(usersByName{users}, repo, follows, nil, NewVisibility(nil, follows, nil))

	// Private profile: owner and follower see it, a stranger is forbidden.
	if v, err := p.Get(context.Background(), "Owner ", &owner); err != nil || !v.IsOwner || len(v.Activities) != 2 || v.Counts.Followers != 2 {
		t.Fatalf("owner %+v %v", v, err)
	}
	if v, err := p.Get(context.Background(), "owner", &follower); err != nil || !v.IsFollowing || len(v.Activities) != 1 {
		t.Fatalf("follower %+v %v", v, err)
	}
	if _, err := p.Get(context.Background(), "owner", &stranger); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger: %v", err)
	}
	if _, err := p.Get(context.Background(), "nobody", nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	// Public profile: anonymous sees the public activity.
	u := users.users[owner]
	u.ProfileIsPublic = true
	users.users[owner] = u
	if v, err := p.Get(context.Background(), "owner", nil); err != nil || v.IsOwner || len(v.Activities) != 1 {
		t.Fatalf("anonymous %+v %v", v, err)
	}
}

// usersByName adds username lookup to fakeUsers.
type usersByName struct{ *fakeUsers }

func (u usersByName) GetUserByUsername(_ context.Context, name string) (domain.User, error) {
	for _, x := range u.users {
		if x.Username == name {
			return x, nil
		}
	}
	return domain.User{}, domain.ErrNotFound
}
