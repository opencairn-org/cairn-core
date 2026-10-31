package view

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type listingFollows struct {
	fakeFollows
	followers, following []domain.UserID
}

func (f *listingFollows) ListFollowers(_ context.Context, _ domain.UserID, limit, offset int) ([]domain.UserID, error) {
	return page(f.followers, limit, offset), nil
}
func (f *listingFollows) ListFollowing(_ context.Context, _ domain.UserID, limit, offset int) ([]domain.UserID, error) {
	return page(f.following, limit, offset), nil
}

func page(ids []domain.UserID, limit, offset int) []domain.UserID {
	if offset >= len(ids) {
		return nil
	}
	return ids[offset:min(offset+limit, len(ids))]
}

type fakeBlocks struct {
	port.BlockRepo
	blocked []domain.UserID
}

func (f *fakeBlocks) ListBlocked(context.Context, domain.UserID) ([]domain.UserID, error) {
	return f.blocked, nil
}
func (f *fakeBlocks) IsBlockedEitherWay(_ context.Context, a, b domain.UserID) (bool, error) {
	return slices.Contains(f.blocked, a) || slices.Contains(f.blocked, b), nil
}

func TestProfilesFollowListsAndBlocks(t *testing.T) {
	ctx := t.Context()
	me, alice, bob, gone := domain.UserID(uuid.New()), domain.UserID(uuid.New()), domain.UserID(uuid.New()), domain.UserID(uuid.New())
	users := &fakeUsers{users: map[domain.UserID]domain.User{me: {ID: me, Username: "me"}, alice: {ID: alice, Username: "alice"}, bob: {ID: bob, Username: "bob"}}}
	follows := &listingFollows{fakeFollows: fakeFollows{following: map[domain.UserID]map[domain.UserID]bool{me: {alice: true}}},
		followers: []domain.UserID{bob, gone, alice}, following: []domain.UserID{alice}}
	p := NewProfiles(users, nil, follows, &fakeBlocks{blocked: []domain.UserID{bob, gone}}, nil)

	st, err := p.FollowState(ctx, me, alice)
	if err != nil || !st.Following || st.Counts.Followers != 2 || st.IsSelf {
		t.Fatalf("state: %v %+v", err, st)
	}
	if st, _ := p.FollowState(ctx, me, me); !st.IsSelf || st.Following {
		t.Fatalf("self: %+v", st)
	}
	// Unknown users are dropped from the cards, paging is passed through.
	cards, err := p.Followers(ctx, me, alice, 10, 0)
	if err != nil || len(cards) != 2 || cards[0].Username != "bob" || cards[1].Username != "alice" {
		t.Fatalf("followers: %v %+v", err, cards)
	}
	if cards, _ := p.Followers(ctx, me, alice, 10, 2); len(cards) != 1 || cards[0].ID != alice {
		t.Fatalf("followers page: %+v", cards)
	}
	if cards, _ := p.Following(ctx, me, me, 10, 0); len(cards) != 1 || cards[0].ID != alice {
		t.Fatalf("following: %+v", cards)
	}
	// A blocked user's graph is gone; a private profile is for followers only.
	if _, err := p.Followers(ctx, me, bob, 10, 0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("blocked graph: %v", err)
	}
	stranger := domain.UserID(uuid.New())
	if _, err := p.Following(ctx, stranger, alice, 10, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("private graph: %v", err)
	}
	if cards, _ := p.Blocked(ctx, me); len(cards) != 1 || cards[0].ID != bob {
		t.Fatalf("blocked: %+v", cards)
	}
	bare := NewProfiles(users, nil, nil, nil, nil)
	if _, err := bare.FollowState(ctx, me, alice); err == nil {
		t.Fatal("no follows → unavailable")
	}
	if _, err := bare.Blocked(ctx, me); err == nil {
		t.Fatal("no blocks → unavailable")
	}
}
