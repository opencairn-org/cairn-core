package view

import (
	"context"
	"fmt"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ErrForbidden: the record exists but is not shared with this caller.
var ErrForbidden = port.NewActionError(port.ActionErrorForbidden, "not permitted")

// Project renders activities for viewer (nil = anonymous): the viewer's own
// rows stay full activities, everyone else's are projected through the
// visibility model, and rows the viewer may not see are dropped. Owner
// records and privacy zones are cached per owner.
func (f *Feeds) Project(ctx context.Context, acts []domain.Activity, viewer *domain.UserID) []FeedItem {
	owners := map[domain.UserID]domain.User{}
	zones := map[domain.UserID][]domain.PrivacyZone{}
	out := make([]FeedItem, 0, len(acts))
	for i := range acts {
		a := acts[i]
		isOwner := viewer != nil && *viewer == a.UserID
		cats := f.visibility.For(ctx, a.UserID, viewer, a.ID, false)
		if !isOwner && len(cats) == 0 {
			continue
		}
		if _, cached := owners[a.UserID]; !cached {
			owners[a.UserID], _ = f.users.GetUser(ctx, a.UserID)
			zones[a.UserID] = f.visibility.OwnerZones(ctx, a.UserID, isOwner)
		}
		item := FeedItem{Source: FeedFollowing, At: a.StartTime, Owner: owners[a.UserID]}
		if isOwner {
			item.Source, item.Own = FeedSelf, &a
		} else {
			p := domain.ProjectActivity(a, cats, zones[a.UserID])
			item.Projected = &p
		}
		out = append(out, item)
	}
	return out
}

// Projected renders one activity for viewer: owners get the full projection,
// others only what the owner shares; a private or hidden activity, a block
// or an empty grant is ErrForbidden; an unknown id is domain.ErrNotFound.
func (f *Feeds) Projected(ctx context.Context, id domain.ActivityID, viewer *domain.UserID, hasLink bool) (domain.ProjectedActivity, domain.User, bool, error) {
	a, err := f.activities.GetActivity(ctx, id)
	if err != nil {
		return domain.ProjectedActivity{}, domain.User{}, false, domain.ErrNotFound
	}
	isOwner := viewer != nil && *viewer == a.UserID
	if !isOwner && !hasLink && (a.Privacy == domain.PrivacyPrivate || a.HiddenByAdmin) {
		return domain.ProjectedActivity{}, domain.User{}, false, ErrForbidden
	}
	if hasLink && a.HiddenByAdmin {
		return domain.ProjectedActivity{}, domain.User{}, false, domain.ErrNotFound
	}
	cats := f.visibility.For(ctx, a.UserID, viewer, a.ID, hasLink)
	if !isOwner && len(cats) == 0 {
		return domain.ProjectedActivity{}, domain.User{}, false, ErrForbidden
	}
	owner, _ := f.users.GetUser(ctx, a.UserID)
	return domain.ProjectActivity(a, cats, f.visibility.OwnerZones(ctx, a.UserID, isOwner)), owner, isOwner, nil
}

// Shared resolves a share link: the token is the authorization and resolves
// as the "link" audience (which the owner's policy may restrict further).
func (f *Feeds) Shared(ctx context.Context, links port.ShareLinkRepo, token string, viewer *domain.UserID) (domain.ProjectedActivity, domain.User, error) {
	if links == nil {
		return domain.ProjectedActivity{}, domain.User{}, ErrUnavailable
	}
	link, err := links.GetActive(ctx, token)
	if err != nil {
		return domain.ProjectedActivity{}, domain.User{}, domain.ErrNotFound
	}
	p, owner, _, err := f.Projected(ctx, link.ActivityID, viewer, true)
	return p, owner, err
}

// ClubCard is a club with its member count.
type ClubCard struct {
	Club    domain.Club
	Members int
}

// ClubDetail is a club as one viewer sees it.
type ClubDetail struct {
	ClubCard
	IsMember bool
	Role     domain.ClubMemberRole
}

// ClubMemberRow is a member with their user record.
type ClubMemberRow struct {
	Member domain.ClubMember
	User   domain.User
}

// Clubs are the club reads (SocialService, GET /api/clubs…).
type Clubs struct {
	clubs port.ClubRepo
	users port.UserRepo
	feeds *Feeds
}

// NewClubs wires the reads; clubs may be nil (→ ErrUnavailable).
func NewClubs(clubs port.ClubRepo, users port.UserRepo, feeds *Feeds) *Clubs {
	return &Clubs{clubs: clubs, users: users, feeds: feeds}
}

// Club paging bounds.
const (
	ClubsDefaultLimit   = 50
	ClubsMaxLimit       = 200
	ClubMembersLimit    = 200
	ClubFeedDefaultSize = 30
	ClubFeedMaxSize     = 100
)

// List is the public clubs plus the viewer's own.
func (c *Clubs) List(ctx context.Context, me domain.UserID, limit, offset int) ([]ClubCard, error) {
	if c.clubs == nil {
		return nil, ErrUnavailable
	}
	if limit <= 0 {
		limit = ClubsDefaultLimit
	}
	if limit > ClubsMaxLimit {
		limit = ClubsMaxLimit
	}
	clubs, err := c.clubs.List(ctx, me, limit, max(0, offset))
	if err != nil {
		return nil, fmt.Errorf("list clubs: %w", err)
	}
	out := make([]ClubCard, 0, len(clubs))
	for _, cl := range clubs {
		n, _ := c.clubs.CountMembers(ctx, cl.ID)
		out = append(out, ClubCard{Club: cl, Members: n})
	}
	return out, nil
}

// Get resolves a club for viewer (nil = anonymous). A private club is
// ErrForbidden to non-members; an unknown slug is domain.ErrNotFound.
func (c *Clubs) Get(ctx context.Context, slug string, viewer *domain.UserID) (ClubDetail, error) {
	if c.clubs == nil {
		return ClubDetail{}, ErrUnavailable
	}
	cl, err := c.clubs.GetBySlug(ctx, slug)
	if err != nil {
		return ClubDetail{}, domain.ErrNotFound
	}
	d := ClubDetail{ClubCard: ClubCard{Club: cl}}
	if viewer != nil {
		if role, member, _ := c.clubs.MemberRole(ctx, cl.ID, *viewer); member {
			d.IsMember, d.Role = true, role
		}
	}
	if !cl.IsPublic && !d.IsMember {
		return ClubDetail{}, ErrForbidden
	}
	d.Members, _ = c.clubs.CountMembers(ctx, cl.ID)
	return d, nil
}

// Members lists a visible club's members with their user records.
func (c *Clubs) Members(ctx context.Context, slug string, viewer *domain.UserID, limit, offset int) ([]ClubMemberRow, error) {
	d, err := c.Get(ctx, slug, viewer)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > ClubMembersLimit {
		limit = ClubMembersLimit
	}
	members, err := c.clubs.ListMembers(ctx, d.Club.ID, limit, max(0, offset))
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	out := make([]ClubMemberRow, 0, len(members))
	for _, m := range members {
		row := ClubMemberRow{Member: m}
		row.User, _ = c.users.GetUser(ctx, m.UserID)
		out = append(out, row)
	}
	return out, nil
}

// Feed lists a visible club's member activities, projected per the
// visibility model (club membership is not an audience of its own).
func (c *Clubs) Feed(ctx context.Context, slug string, viewer *domain.UserID, limit, offset int) ([]FeedItem, error) {
	d, err := c.Get(ctx, slug, viewer)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = ClubFeedDefaultSize
	}
	if limit > ClubFeedMaxSize {
		limit = ClubFeedMaxSize
	}
	acts, err := c.clubs.ListClubFeed(ctx, d.Club.ID, limit, max(0, offset))
	if err != nil {
		return nil, fmt.Errorf("club feed: %w", err)
	}
	return c.feeds.Project(ctx, acts, viewer), nil
}

// ProfileView is a public athlete profile as one viewer sees it.
type ProfileView struct {
	User        domain.User
	IsOwner     bool
	IsFollowing bool
	Counts      domain.FollowCounts
	Activities  []domain.ProjectedActivity
}

// Profiles are the public profile reads (SocialService.GetProfile, GET /api/profiles/{username}).
type Profiles struct {
	users      port.UserRepo
	activities port.ActivityRepo
	follows    port.FollowRepo
	blocks     port.BlockRepo
	visibility *Visibility
}

// NewProfiles wires the reads; follows and blocks may be nil.
func NewProfiles(users port.UserRepo, activities port.ActivityRepo, follows port.FollowRepo, blocks port.BlockRepo, visibility *Visibility) *Profiles {
	return &Profiles{users: users, activities: activities, follows: follows, blocks: blocks, visibility: visibility}
}

// profileRecent is how many recent activities a profile shows.
const profileRecent = 20

// Get resolves username for viewer (nil = anonymous). A blocked or unknown
// user is domain.ErrNotFound; a non-public profile is ErrForbidden unless
// the viewer is the owner or a follower.
func (p *Profiles) Get(ctx context.Context, username string, viewer *domain.UserID) (ProfileView, error) {
	u, err := p.users.GetUserByUsername(ctx, strings.ToLower(strings.TrimSpace(username)))
	if err != nil {
		return ProfileView{}, domain.ErrNotFound
	}
	v := ProfileView{User: u, IsOwner: viewer != nil && *viewer == u.ID}
	if viewer != nil && !v.IsOwner {
		if p.blocks != nil {
			if blocked, _ := p.blocks.IsBlockedEitherWay(ctx, *viewer, u.ID); blocked {
				return ProfileView{}, domain.ErrNotFound
			}
		}
		if p.follows != nil {
			v.IsFollowing, _ = p.follows.IsFollowing(ctx, *viewer, u.ID)
		}
	}
	if !u.ProfileIsPublic && !v.IsOwner && !v.IsFollowing {
		return ProfileView{}, ErrForbidden
	}
	if p.follows != nil {
		v.Counts, _ = p.follows.Counts(ctx, u.ID)
	}
	v.Activities = p.recent(ctx, u, viewer, v.IsOwner)
	return v, nil
}

// recent projects the owner's newest activities for the viewer: owners see
// all, others only non-private ones the visibility model opens.
func (p *Profiles) recent(ctx context.Context, u domain.User, viewer *domain.UserID, isOwner bool) []domain.ProjectedActivity {
	recent, _ := p.activities.ListRecentActivitiesForUser(ctx, u.ID, profileRecent)
	zones := p.visibility.OwnerZones(ctx, u.ID, isOwner)
	out := make([]domain.ProjectedActivity, 0, len(recent))
	for _, a := range recent {
		if !isOwner && (a.Privacy == domain.PrivacyPrivate || a.HiddenByAdmin) {
			continue
		}
		cats := p.visibility.For(ctx, u.ID, viewer, a.ID, false)
		if !isOwner && len(cats) == 0 {
			continue
		}
		out = append(out, domain.ProjectActivity(a, cats, zones))
	}
	return out
}
