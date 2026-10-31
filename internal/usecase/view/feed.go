package view

import (
	"context"
	"net/url"
	"path"
	"sort"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// FeedSource says where a feed row comes from.
type FeedSource string

const (
	FeedSelf      FeedSource = "self"
	FeedFollowing FeedSource = "following"
	FeedFederated FeedSource = "federated"
)

// FeedItem is one row of a feed. Exactly one of Own, Projected, Federated is set.
type FeedItem struct {
	Source FeedSource
	At     time.Time
	// Owner is set for Self and Following rows.
	Owner     domain.User
	Own       *domain.Activity
	Projected *domain.ProjectedActivity
	Federated *domain.FederatedFeedItem
}

// Feeds assembles the social feeds (GET /api/feed/home, SocialService.ListHomeFeed).
type Feeds struct {
	activities        port.ActivityReader
	users             port.UserReader
	visibility        *Visibility
	federated         port.FederationFeedRepo // optional
	federationEnabled bool
}

// NewFeeds wires the feeds. federated may be nil; federationEnabled gates
// federated rows on the instance flag, not just the wired repo, so items
// received while federation was on stay dormant when it is switched off.
func NewFeeds(activities port.ActivityReader, users port.UserReader, visibility *Visibility, federated port.FederationFeedRepo, federationEnabled bool) *Feeds {
	return &Feeds{activities: activities, users: users, visibility: visibility, federated: federated, federationEnabled: federationEnabled}
}

// Paging bounds of the home feed.
const (
	FeedDefaultLimit = 30
	FeedMaxLimit     = 100
)

// Home merges the caller's own activities, the local following feed
// (projected through the visibility model) and federated rows, newest first,
// capped at limit. offset pages the following and federated sources.
func (f *Feeds) Home(ctx context.Context, me domain.UserID, limit, offset int) []FeedItem {
	limit, offset = feedPage(limit, offset)
	var items []FeedItem
	items = append(items, f.own(ctx, me, limit)...)
	items = append(items, f.Following(ctx, me, limit, offset)...)
	if f.federationEnabled && f.federated != nil {
		items = append(items, f.federatedRows(ctx, me, limit, offset)...)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].At.After(items[j].At) })
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

// Following is the local following feed alone: friends on this server,
// projected per the visibility model. Rows the viewer may not see are dropped.
func (f *Feeds) Following(ctx context.Context, me domain.UserID, limit, offset int) []FeedItem {
	limit, offset = feedPage(limit, offset)
	acts, _ := f.activities.ListFollowingFeed(ctx, me, limit, offset)
	owners := map[domain.UserID]domain.User{}
	zones := map[domain.UserID][]domain.PrivacyZone{}
	var items []FeedItem
	for _, a := range acts {
		cats := f.visibility.For(ctx, &a, &me, false)
		if len(cats) == 0 {
			continue
		}
		if _, cached := owners[a.UserID]; !cached {
			owners[a.UserID], _ = f.users.GetUser(ctx, a.UserID)
			zones[a.UserID] = f.visibility.OwnerZones(ctx, a.UserID, false)
		}
		p := domain.ProjectActivity(a, cats, zones[a.UserID])
		items = append(items, FeedItem{Source: FeedFollowing, At: a.StartTime, Owner: owners[a.UserID], Projected: &p})
	}
	return items
}

func (f *Feeds) own(ctx context.Context, me domain.UserID, limit int) []FeedItem {
	meUser, _ := f.users.GetUser(ctx, me)
	own, _ := f.activities.ListRecentActivitiesForUser(ctx, me, limit)
	items := make([]FeedItem, 0, len(own))
	for i := range own {
		a := own[i]
		items = append(items, FeedItem{Source: FeedSelf, At: a.StartTime, Owner: meUser, Own: &a})
	}
	return items
}

func (f *Feeds) federatedRows(ctx context.Context, me domain.UserID, limit, offset int) []FeedItem {
	rows, _ := f.federated.ListForUser(ctx, me, limit, offset)
	items := make([]FeedItem, 0, len(rows))
	for i := range rows {
		it := rows[i]
		items = append(items, FeedItem{Source: FeedFederated, At: it.Published, Federated: &it})
	}
	return items
}

func feedPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = FeedDefaultLimit
	}
	if limit > FeedMaxLimit {
		limit = FeedMaxLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// RemoteHandle renders an actor URL as @user@host for display.
func RemoteHandle(actorID string) string {
	if u, err := url.Parse(actorID); err == nil && u.Host != "" {
		return "@" + path.Base(u.Path) + "@" + u.Host
	}
	return actorID
}
