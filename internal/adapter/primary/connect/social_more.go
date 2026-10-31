package connect

import (
	"context"
	"fmt"

	connectrpc "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// feedItems converts projected/own rows to wire feed items.
func (s *SocialServer) feedItems(ctx context.Context, items []view.FeedItem) []*v1.FeedItem {
	out := make([]*v1.FeedItem, 0, len(items))
	for _, it := range items {
		out = append(out, s.feedItemToProto(ctx, it))
	}
	return out
}

func (s *SocialServer) ListFollowingFeed(ctx context.Context, req *v1.ListFollowingFeedRequest) (*v1.ListFollowingFeedResponse, error) {
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
	items := s.Feeds.Following(ctx, me, limit, offset)
	out := &v1.ListFollowingFeedResponse{Items: s.feedItems(ctx, items), Page: &v1.PageResponse{Total: -1}}
	if len(items) == limit {
		out.Page.NextCursor = encodeOffset(offset + limit)
	}
	return out, nil
}

func (s *SocialServer) clubs() (*view.Clubs, error) {
	if s.Clubs == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "clubs are not available on this instance")
	}
	return s.Clubs, nil
}

func clubCardToProto(c view.ClubCard) *v1.ClubCard {
	return &v1.ClubCard{Id: c.Club.ID.String(), Slug: c.Club.Slug, Name: c.Club.Name, IsPublic: c.Club.IsPublic,
		MemberCount: connectClampInt32(c.Members), CreatedAt: timestamppb.New(c.Club.CreatedAt)}
}

func (s *SocialServer) ListClubs(ctx context.Context, req *v1.ListClubsRequest) (*v1.ListClubsResponse, error) {
	me, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	clubs, err := s.clubs()
	if err != nil {
		return nil, err
	}
	limit, offset := pageFrom(req.GetPage())
	cards, err := clubs.List(ctx, me, limit, offset)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListClubsResponse{Page: &v1.PageResponse{Total: -1}}
	for _, c := range cards {
		out.Clubs = append(out.Clubs, clubCardToProto(c))
	}
	if len(cards) == limit {
		out.Page.NextCursor = encodeOffset(offset + limit)
	}
	return out, nil
}

func (s *SocialServer) GetClub(ctx context.Context, req *v1.GetClubRequest) (*v1.GetClubResponse, error) {
	clubs, err := s.clubs()
	if err != nil {
		return nil, err
	}
	d, err := clubs.Get(ctx, req.GetSlug(), viewerFor(ctx))
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.GetClubResponse{Club: clubCardToProto(d.ClubCard), Description: d.Club.Description, IsMember: d.IsMember, MyRole: string(d.Role)}, nil
}

func (s *SocialServer) ListClubMembers(ctx context.Context, req *v1.ListClubMembersRequest) (*v1.ListClubMembersResponse, error) {
	clubs, err := s.clubs()
	if err != nil {
		return nil, err
	}
	limit, offset := pageFrom(req.GetPage())
	rows, err := clubs.Members(ctx, req.GetSlug(), viewerFor(ctx), limit, offset)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListClubMembersResponse{Page: &v1.PageResponse{Total: -1}}
	for _, m := range rows {
		out.Members = append(out.Members, &v1.ClubMember{User: userCardToProto(m.User), Role: string(m.Member.Role)})
	}
	if len(rows) == limit {
		out.Page.NextCursor = encodeOffset(offset + limit)
	}
	return out, nil
}

func (s *SocialServer) ListClubFeed(ctx context.Context, req *v1.ListClubFeedRequest) (*v1.ListClubFeedResponse, error) {
	clubs, err := s.clubs()
	if err != nil {
		return nil, err
	}
	limit, offset := pageFrom(req.GetPage())
	if req.GetPage().GetLimit() <= 0 {
		limit = view.ClubFeedDefaultSize
	}
	items, err := clubs.Feed(ctx, req.GetSlug(), viewerFor(ctx), limit, offset)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListClubFeedResponse{Items: s.feedItems(ctx, items), Page: &v1.PageResponse{Total: -1}}
	if len(items) == limit {
		out.Page.NextCursor = encodeOffset(offset + limit)
	}
	return out, nil
}

func (s *SocialServer) GetProfile(ctx context.Context, req *v1.GetProfileRequest) (*v1.GetProfileResponse, error) {
	if s.Profiles == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "profiles are not available on this instance")
	}
	pv, err := s.Profiles.Get(ctx, req.GetUsername(), viewerFor(ctx))
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.GetProfileResponse{
		User: userCardToProto(pv.User), IsPublic: pv.User.ProfileIsPublic, IsSelf: pv.IsOwner, IsFollowing: pv.IsFollowing,
		FollowersCount: connectClampInt32(pv.Counts.Followers), FollowingCount: connectClampInt32(pv.Counts.Following),
	}
	for _, p := range pv.Activities {
		out.Activities = append(out.Activities, s.projectedToProto(ctx, p))
	}
	return out, nil
}

func (s *SocialServer) GetProjectedActivity(ctx context.Context, req *v1.GetProjectedActivityRequest) (*v1.GetProjectedActivityResponse, error) {
	if s.Feeds == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "feeds are not available on this instance")
	}
	id, err := parseActivityID(req.GetActivityId())
	if err != nil {
		return nil, err
	}
	p, owner, isOwner, err := s.Feeds.Projected(ctx, id, viewerFor(ctx), false)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.GetProjectedActivityResponse{Activity: s.projectedToProto(ctx, p), Owner: userCardToProto(owner), IsOwner: isOwner}, nil
}

func (s *SocialServer) GetSharedActivity(ctx context.Context, req *v1.GetSharedActivityRequest) (*v1.GetSharedActivityResponse, error) {
	if s.Feeds == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "feeds are not available on this instance")
	}
	p, owner, err := s.Feeds.Shared(ctx, s.ShareLinks, req.GetToken(), viewerFor(ctx))
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.GetSharedActivityResponse{Activity: s.projectedToProto(ctx, p), Owner: userCardToProto(owner)}, nil
}

func (s *SocialServer) ListReviewQueue(ctx context.Context, _ *v1.ListReviewQueueRequest) (*v1.ListReviewQueueResponse, error) {
	me, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Activities == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "the review queue is not available on this instance")
	}
	items, err := s.Activities.ListActivitiesNeedingReview(ctx, me)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("review queue: %w", err))
	}
	out := &v1.ListReviewQueueResponse{}
	for _, it := range items {
		item := &v1.ReviewItem{Id: it.ID.String(), Title: it.Title, Type: activityTypeToProto(it.Type), StartTime: timestamppb.New(it.StartTime), MatchConfidence: it.MatchConfidence, Kind: string(it.Kind)}
		if it.Kind == domain.ReviewKindDuplicate {
			item.DuplicateOfId = it.DuplicateOf.String()
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Compile-time use of the port types the server's optional fields hold.
var (
	_ port.ShareLinkRepo = (port.ShareLinkRepo)(nil)
	_                    = domain.ActivityReviewItem{}
)
