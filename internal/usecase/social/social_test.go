package social

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

func kindOf(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("want ActionError, got %v", err)
	}
	return ae.Kind
}

func newUser() domain.UserID { return domain.UserID(uuid.New()) }

type fakeFollows struct {
	port.FollowRepo
	edges map[[2]domain.UserID]bool
}

func (f *fakeFollows) Follow(_ context.Context, a, b domain.UserID) error {
	f.edges[[2]domain.UserID{a, b}] = true
	return nil
}

func (f *fakeFollows) Unfollow(_ context.Context, a, b domain.UserID) error {
	delete(f.edges, [2]domain.UserID{a, b})
	return nil
}

type fakeBlocks struct {
	port.BlockRepo
	blocked bool
}

func (f *fakeBlocks) IsBlockedEitherWay(context.Context, domain.UserID, domain.UserID) (bool, error) {
	return f.blocked, nil
}

func TestGraphFollow(t *testing.T) {
	ctx := t.Context()
	me, other := newUser(), newUser()
	follows := &fakeFollows{edges: map[[2]domain.UserID]bool{}}
	blocks := &fakeBlocks{}
	g := &Graph{Follows: follows, Blocks: blocks}

	if k := kindOf(t, g.Follow(ctx, me, me)); k != port.ActionErrorInvalid {
		t.Fatalf("self-follow: %v", k)
	}
	blocks.blocked = true
	if k := kindOf(t, g.Follow(ctx, me, other)); k != port.ActionErrorForbidden {
		t.Fatalf("blocked: %v", k)
	}
	blocks.blocked = false
	if err := g.Follow(ctx, me, other); err != nil || !follows.edges[[2]domain.UserID{me, other}] {
		t.Fatalf("follow: %v %v", err, follows.edges)
	}
	if err := g.Unfollow(ctx, me, other); err != nil || len(follows.edges) != 0 {
		t.Fatalf("unfollow: %v %v", err, follows.edges)
	}
	// Without a block repo there is nothing to check.
	if err := (&Graph{Follows: follows}).Follow(ctx, me, other); err != nil {
		t.Fatal(err)
	}
}

type fakeVisibility struct {
	port.VisibilityRepo
	policy domain.VisibilityPolicy
	zone   domain.PrivacyZone
}

func (f *fakeVisibility) GetUserPolicy(context.Context, domain.UserID) (domain.VisibilityPolicy, error) {
	return f.policy, nil
}

func (f *fakeVisibility) AddPrivacyZone(_ context.Context, z domain.PrivacyZone) (domain.PrivacyZoneID, error) {
	f.zone = z
	return domain.PrivacyZoneID(uuid.New()), nil
}

func TestPrivacyDefaults(t *testing.T) {
	ctx := t.Context()
	me := newUser()
	vis := &fakeVisibility{}
	p := &Privacy{Visibility: vis}

	pol, err := p.Policy(ctx, me)
	if err != nil || pol.ByAudience == nil {
		t.Fatalf("unset policy must fall back to the default: %v %+v", err, pol)
	}
	if _, err := p.AddZone(ctx, me, domain.PrivacyZone{UserID: newUser(), Lat: 1, Lng: 2}); err != nil {
		t.Fatal(err)
	}
	if vis.zone.UserID != me || vis.zone.RadiusM != defaultZoneRadiusM {
		t.Fatalf("zone = %+v", vis.zone)
	}
}

type fakeClubs struct {
	port.ClubRepo
	clubs   map[string]domain.Club
	members map[domain.UserID]bool
}

func (f *fakeClubs) GetBySlug(_ context.Context, slug string) (domain.Club, error) {
	c, ok := f.clubs[slug]
	if !ok {
		return domain.Club{}, domain.ErrNotFound
	}
	return c, nil
}

func (f *fakeClubs) Create(_ context.Context, c domain.Club) (domain.ClubID, error) {
	c.ID = domain.ClubID(uuid.New())
	f.clubs[c.Slug] = c
	return c.ID, nil
}

func (f *fakeClubs) AddMember(_ context.Context, _ domain.ClubID, u domain.UserID, _ domain.ClubMemberRole) error {
	f.members[u] = true
	return nil
}

func (f *fakeClubs) RemoveMember(_ context.Context, _ domain.ClubID, u domain.UserID) error {
	delete(f.members, u)
	return nil
}

func TestClubs(t *testing.T) {
	ctx := t.Context()
	owner, other := newUser(), newUser()
	repo := &fakeClubs{clubs: map[string]domain.Club{}, members: map[domain.UserID]bool{}}
	c := &Clubs{Repo: repo}

	for _, name := range []string{"  ", "!!!"} {
		if _, _, err := c.Create(ctx, owner, NewClub{Name: name}); kindOf(t, err) != port.ActionErrorInvalid {
			t.Fatalf("%q: %v", name, err)
		}
	}
	_, slug, err := c.Create(ctx, owner, NewClub{Name: " Morning Riders! ", Description: strings.Repeat("x", domain.MaxClubDescriptionLength+5)})
	if err != nil || slug != "morning-riders" {
		t.Fatalf("create: %q %v", slug, err)
	}
	if got := repo.clubs[slug]; !got.IsPublic || len(got.Description) != domain.MaxClubDescriptionLength || got.OwnerID != owner {
		t.Fatalf("club = %+v", got)
	}
	_, slug2, _ := c.Create(ctx, owner, NewClub{Name: "Morning Riders"})
	if slug2 != slug+"-"+owner.String()[:8] {
		t.Fatalf("collision slug = %q", slug2)
	}

	if err := c.Join(ctx, other, slug); err != nil || !repo.members[other] {
		t.Fatalf("join: %v", err)
	}
	if err := c.Leave(ctx, other, slug); err != nil || repo.members[other] {
		t.Fatalf("leave: %v", err)
	}
	if k := kindOf(t, c.Leave(ctx, owner, slug)); k != port.ActionErrorInvalid {
		t.Fatalf("owner leave: %v", k)
	}
	if k := kindOf(t, c.Join(ctx, other, "nope")); k != port.ActionErrorNotFound {
		t.Fatalf("missing club: %v", k)
	}
	private := false
	_, ps, _ := c.Create(ctx, owner, NewClub{Name: "Secret", IsPublic: &private})
	if k := kindOf(t, c.Join(ctx, other, ps)); k != port.ActionErrorForbidden {
		t.Fatalf("private join: %v", k)
	}
}

type fakeActivities struct {
	port.ActivityReader
	acts map[domain.ActivityID]domain.Activity
}

func (f *fakeActivities) GetActivity(_ context.Context, id domain.ActivityID) (domain.Activity, error) {
	a, ok := f.acts[id]
	if !ok {
		return domain.Activity{}, errors.New("no rows")
	}
	return a, nil
}

type fakeGate struct{ allow bool }

func (g fakeGate) SocialAllowed(context.Context, domain.Activity, domain.UserID) bool { return g.allow }

type fakeEngagement struct {
	port.EngagementRepo
	kudos   map[domain.UserID]bool
	comment domain.Comment
}

func (f *fakeEngagement) AddKudos(_ context.Context, _ domain.ActivityID, u domain.UserID) error {
	f.kudos[u] = true
	return nil
}

func (f *fakeEngagement) RemoveKudos(_ context.Context, _ domain.ActivityID, u domain.UserID) error {
	delete(f.kudos, u)
	return nil
}

func (f *fakeEngagement) CountKudos(context.Context, domain.ActivityID) (int, error) {
	return len(f.kudos), nil
}

func (f *fakeEngagement) AddComment(_ context.Context, c domain.Comment) (domain.CommentID, error) {
	f.comment = c
	return domain.CommentID(uuid.New()), nil
}

func TestEngagement(t *testing.T) {
	ctx := t.Context()
	me := newUser()
	act := domain.Activity{ID: domain.ActivityID(uuid.New()), UserID: newUser()}
	repo := &fakeEngagement{kudos: map[domain.UserID]bool{}}
	e := &Engagement{
		Activities: &fakeActivities{acts: map[domain.ActivityID]domain.Activity{act.ID: act}},
		Repo:       repo, Gate: fakeGate{allow: true},
	}

	if _, err := e.Activity(ctx, me, domain.ActivityID(uuid.New())); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing activity: %v", err)
	}
	if _, err := (&Engagement{Activities: e.Activities, Gate: fakeGate{}}).Activity(ctx, me, act.ID); kindOf(t, err) != port.ActionErrorForbidden {
		t.Fatalf("gated: %v", err)
	}
	a, err := e.Activity(ctx, me, act.ID)
	if err != nil || a.ID != act.ID {
		t.Fatalf("allowed: %v", err)
	}
	if n, err := e.AddKudos(ctx, a, me); err != nil || n != 1 {
		t.Fatalf("add kudos: %d %v", n, err)
	}
	if n, err := e.RemoveKudos(ctx, a, me); err != nil || n != 0 {
		t.Fatalf("remove kudos: %d %v", n, err)
	}
	if _, err := e.AddComment(ctx, a, me, "   "); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("empty comment: %v", err)
	}
	long := strings.Repeat("ü", domain.MaxCommentLength) // 2 bytes per rune
	if _, err := e.AddComment(ctx, a, me, "  "+long); err != nil {
		t.Fatal(err)
	}
	if b := repo.comment.Body; len(b) != domain.MaxCommentLength || !strings.HasPrefix(b, "ü") || repo.comment.UserID != me {
		t.Fatalf("comment = %d bytes, %+v", len(b), repo.comment.UserID)
	}
}

func TestTruncateUTF8(t *testing.T) {
	if got := TruncateUTF8("aü", 2); got != "a" {
		t.Fatalf("got %q", got)
	}
	if got := TruncateUTF8("abc", 5); got != "abc" {
		t.Fatalf("got %q", got)
	}
}
