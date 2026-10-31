package connect

import (
	"context"

	connectrpc "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	gearuc "github.com/opencairn-org/cairn-core/internal/usecase/gear"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// SocialServer implements cairnv1connect.SocialServiceHandler: reads across
// users, every non-owner activity projected through the visibility model.
type SocialServer struct {
	cairnv1connect.UnimplementedSocialServiceHandler

	Feeds   *view.Feeds
	Content port.ContentSigner // optional; nil → no map image URLs
	Gear    *gearuc.Service    // optional; gear names on own rows
	MapETag func(domain.Activity) string

	// Clubs, Profiles, ShareLinks and Activities back the club, profile,
	// shared-activity and review-queue reads; nil → Unavailable.
	Clubs      *view.Clubs
	Profiles   *view.Profiles
	ShareLinks port.ShareLinkRepo
	Activities port.ActivityReviewQueue
}

var _ cairnv1connect.SocialServiceHandler = (*SocialServer)(nil)

// NewSocialServer wires the server.
func NewSocialServer(feeds *view.Feeds, content port.ContentSigner, gear *gearuc.Service, mapETag func(domain.Activity) string) *SocialServer {
	return &SocialServer{Feeds: feeds, Content: content, Gear: gear, MapETag: mapETag}
}

func (s *SocialServer) ListHomeFeed(ctx context.Context, req *v1.ListHomeFeedRequest) (*v1.ListHomeFeedResponse, error) {
	me, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Feeds == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "feeds are not available on this instance")
	}
	limit, offset := pageFrom(req.GetPage())
	if req.GetPage().GetLimit() <= 0 {
		limit = view.FeedDefaultLimit
	}
	if limit > view.FeedMaxLimit {
		limit = view.FeedMaxLimit
	}
	items := s.Feeds.Home(ctx, me, limit, offset)
	out := &v1.ListHomeFeedResponse{Items: make([]*v1.FeedItem, 0, len(items)), Page: &v1.PageResponse{Total: -1}}
	for _, it := range items {
		out.Items = append(out.Items, s.feedItemToProto(ctx, it))
	}
	if len(items) == limit {
		out.Page.NextCursor = encodeOffset(offset + limit)
	}
	return out, nil
}

func (s *SocialServer) feedItemToProto(ctx context.Context, it view.FeedItem) *v1.FeedItem {
	switch {
	case it.Own != nil:
		a := *it.Own
		wire := decorateActivity(ctx, activityToProto(a, nil), a, gearNamesOf(ctx, s.Gear, a), s.Content, s.MapETag)
		return &v1.FeedItem{Source: v1.FeedItem_SOURCE_SELF, Owner: userCardToProto(it.Owner), Item: &v1.FeedItem_Own{Own: wire}}
	case it.Projected != nil:
		return &v1.FeedItem{Source: v1.FeedItem_SOURCE_FOLLOWING, Owner: userCardToProto(it.Owner),
			Item: &v1.FeedItem_Projected{Projected: s.projectedToProto(ctx, *it.Projected)}}
	case it.Federated != nil:
		return &v1.FeedItem{Source: v1.FeedItem_SOURCE_FEDERATED, Owner: remoteCardToProto(it.Federated.ActorID),
			Item: &v1.FeedItem_Federated{Federated: federatedToProto(*it.Federated)}}
	}
	return &v1.FeedItem{}
}

// projectedToProto converts a projection; the map image is the trimmed
// variant (projections are for non-owners by definition).
func (s *SocialServer) projectedToProto(ctx context.Context, p domain.ProjectedActivity) *v1.ProjectedActivity {
	out := projectedActivityToProto(p)
	if p.MapVisible && s.Content != nil {
		if u, err := s.Content.MapImage(ctx, p.ID, true); err == nil {
			out.MapImage = contentURLToProto(u)
		}
	}
	return out
}

func projectedActivityToProto(p domain.ProjectedActivity) *v1.ProjectedActivity {
	out := &v1.ProjectedActivity{
		Id:                    p.ID.String(),
		Title:                 p.Title,
		Type:                  activityTypeToProto(p.Type),
		Discipline:            string(p.Discipline),
		StartTime:             timestamppb.New(p.StartTime),
		Timezone:              p.Timezone,
		ElapsedDurationS:      int64(p.ElapsedDuration.Seconds()),
		MovingDurationS:       int64(p.MovingDuration.Seconds()),
		DistanceM:             p.DistanceM,
		ElevationGainM:        p.ElevationGainM,
		Description:           p.Description,
		Tss:                   p.TSS,
		Calories:              intPtrToInt32(p.CaloriesKcal),
		StartPlace:            p.StartPlace,
		StartLocationRedacted: p.StartLocationRedacted,
		AvgHr:                 intPtrToInt32(p.AvgHeartRateBpm),
		MaxHr:                 intPtrToInt32(p.MaxHeartRateBpm),
		AvgPower:              intPtrToInt32(p.AvgPowerW),
		MaxPower:              intPtrToInt32(p.MaxPowerW),
		NormalizedPower:       intPtrToInt32(p.NormalizedPowerW),
		AvgSpeedMps:           p.AvgSpeedMps,
		MaxSpeedMps:           p.MaxSpeedMps,
		HasPhotos:             p.PhotosVisible,
	}
	if p.StartLat != nil && p.StartLng != nil {
		out.Start = &v1.GeoPoint{Latitude: *p.StartLat, Longitude: *p.StartLng}
	}
	for _, c := range p.Visible {
		out.VisibleCategories = append(out.VisibleCategories, string(c))
	}
	return out
}

func federatedToProto(f domain.FederatedFeedItem) *v1.FederatedActivity {
	out := &v1.FederatedActivity{
		Id:             f.ActivityAPID,
		Title:          f.Name,
		Sport:          f.Sport,
		StartTime:      timestamppb.New(f.Published),
		Summary:        f.Summary,
		Url:            f.URL,
		DistanceM:      f.DistanceM,
		ElevationGainM: f.ElevationM,
	}
	if f.ImageURL != "" {
		out.MapImage = &v1.ContentUrl{Url: f.ImageURL}
	}
	if f.DurationS != nil {
		d := int64(*f.DurationS)
		out.ElapsedDurationS = &d
	}
	return out
}

func userCardToProto(u domain.User) *v1.UserCard {
	if u.ID == (domain.UserID{}) {
		return nil
	}
	return &v1.UserCard{Id: u.ID.String(), Username: u.Username, DisplayName: u.DisplayName, AvatarUrl: u.AvatarURL}
}

func remoteCardToProto(actorID string) *v1.UserCard {
	handle := view.RemoteHandle(actorID)
	return &v1.UserCard{Username: handle, DisplayName: handle, Remote: true, Actor: actorID}
}
