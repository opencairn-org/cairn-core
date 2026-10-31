package invite

import (
	"context"
	"crypto/sha256"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/admin"
)

// UserCreator provisions the account; admin.CreateUser satisfies it.
type UserCreator interface {
	Execute(ctx context.Context, in admin.CreateUserInput) (admin.CreateUserResult, error)
}

// RedeemInvite turns a valid invite code into a new user account. The invite is
// claimed atomically (race-safe) before the user is created; if creation fails
// the claim is released so the code can be retried.
type RedeemInvite struct {
	invites    port.InviteRepo
	createUser UserCreator
	now        func() time.Time
}

func NewRedeemInvite(invites port.InviteRepo, createUser UserCreator, now func() time.Time) *RedeemInvite {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &RedeemInvite{invites: invites, createUser: createUser, now: now}
}

// RedeemInviteInput is the signup form.
type RedeemInviteInput struct {
	Code        string
	Username    string
	Email       string // used only when the invite doesn't pin one
	Password    string
	DisplayName string
}

// Execute claims the invite and provisions the user with the invite's role.
// Returns domain.ErrInviteInvalid for a bad/used/expired code, or the
// underlying user-shape / uniqueness error from user creation.
func (uc *RedeemInvite) Execute(ctx context.Context, in RedeemInviteInput) (domain.User, error) {
	sum := sha256.Sum256([]byte(in.Code))

	claimed, err := uc.invites.ClaimByCodeHash(ctx, sum[:], uc.now())
	if err != nil {
		return domain.User{}, err // ErrInviteInvalid
	}

	// A pinned invite email always wins; the user can't redeem to a different
	// address. An email-pinned invite is treated as verified.
	email := in.Email
	emailVerified := false
	if claimed.Email != "" {
		email = claimed.Email
		emailVerified = true
	}

	res, err := uc.createUser.Execute(ctx, admin.CreateUserInput{
		Username:      in.Username,
		Email:         email,
		DisplayName:   in.DisplayName,
		Password:      in.Password,
		Role:          claimed.AssignedRole,
		EmailVerified: emailVerified,
	})
	if err != nil {
		// Give the code back so the user can fix their input and retry.
		_ = uc.invites.Release(ctx, claimed.ID)
		return domain.User{}, err
	}
	_ = uc.invites.SetUsedBy(ctx, claimed.ID, res.User.ID) // best-effort attribution
	return res.User, nil
}
