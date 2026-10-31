package invite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/admin"
)

type claimInvites struct {
	port.InviteRepo
	claimed  domain.Invite
	released bool
	usedBy   domain.UserID
}

func (c *claimInvites) ClaimByCodeHash(context.Context, []byte, time.Time) (domain.Invite, error) {
	return c.claimed, nil
}
func (c *claimInvites) Release(context.Context, domain.InviteID) error { c.released = true; return nil }
func (c *claimInvites) SetUsedBy(_ context.Context, _ domain.InviteID, u domain.UserID) error {
	c.usedBy = u
	return nil
}

type fakeCreator struct {
	got admin.CreateUserInput
	err error
}

func (f *fakeCreator) Execute(_ context.Context, in admin.CreateUserInput) (admin.CreateUserResult, error) {
	f.got = in
	if f.err != nil {
		return admin.CreateUserResult{}, f.err
	}
	return admin.CreateUserResult{User: domain.User{ID: domain.UserID(uuid.New()), Email: in.Email}}, nil
}

func TestRedeemInvite(t *testing.T) {
	ctx := t.Context()
	repo := &claimInvites{claimed: domain.Invite{ID: domain.InviteID(uuid.New()), Email: "pinned@x.test", AssignedRole: domain.UserRoleUser}}
	creator := &fakeCreator{}
	u, err := NewRedeemInvite(repo, creator, nil).Execute(ctx, RedeemInviteInput{Code: "c", Username: "u", Email: "other@x.test"})
	if err != nil || creator.got.Email != "pinned@x.test" || !creator.got.EmailVerified || repo.usedBy != u.ID || repo.released {
		t.Fatalf("pinned redeem: %v %+v", err, creator.got)
	}
	// A failed creation gives the code back.
	repo = &claimInvites{claimed: domain.Invite{ID: domain.InviteID(uuid.New())}}
	boom := errors.New("taken")
	if _, err := NewRedeemInvite(repo, &fakeCreator{err: boom}, nil).Execute(ctx, RedeemInviteInput{Code: "c"}); !errors.Is(err, boom) || !repo.released {
		t.Fatalf("failed create: %v released=%v", err, repo.released)
	}
}
