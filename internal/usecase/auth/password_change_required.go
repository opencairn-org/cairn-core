package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// PasswordChangeRequired enforces users.must_change_password at sign-in: a
// password login of a flagged user yields a challenge instead of a session,
// and only a new password completes it. Admin resets and operator-typed
// initial passwords set the flag; UpdatePassword clears it.
type PasswordChangeRequired struct {
	Challenges port.LoginChallengeRepo
	Users      port.UserRepo
	Hasher     interface {
		Hash(password string) (string, error)
	}
	Now func() time.Time
}

func (p *PasswordChangeRequired) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// Required reports whether the user must replace the password before signing in.
func (p *PasswordChangeRequired) Required(u domain.User) bool {
	return p != nil && u.MustChangePassword
}

// Begin stores a password-change challenge and returns its one-time token.
func (p *PasswordChangeRequired) Begin(ctx context.Context, userID domain.UserID) (string, error) {
	return beginChallenge(ctx, p.Challenges, userID, domain.ChallengePasswordChange, p.now())
}

// Complete sets the new password, clears the flag and consumes the
// challenge; it returns the user so the transport can continue the sign-in.
func (p *PasswordChangeRequired) Complete(ctx context.Context, token, next string) (domain.User, error) {
	if err := domain.ValidatePassword(next); err != nil {
		return domain.User{}, port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	hash := sha256.Sum256([]byte(token))
	now := p.now()
	c, err := p.Challenges.GetByTokenHash(ctx, hash[:])
	if err != nil || c.Purpose != domain.ChallengePasswordChange || !c.Open(now) {
		return domain.User{}, port.NewActionError(port.ActionErrorInvalid, ErrChallengeInvalid.Error())
	}
	user, err := p.Users.GetUser(ctx, c.UserID)
	if err != nil {
		return domain.User{}, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("load user: %v", err))
	}
	if !user.MustChangePassword {
		return domain.User{}, port.NewActionError(port.ActionErrorPrecondition, "this account no longer needs a password change; sign in again")
	}
	encoded, err := p.Hasher.Hash(next)
	if err != nil {
		return domain.User{}, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("hash password: %v", err))
	}
	if err := p.Users.UpdatePassword(ctx, user.ID, encoded); err != nil {
		return domain.User{}, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("update password: %v", err))
	}
	// Satisfied by the new password, consumed right away: the token is single-use.
	if err := p.Challenges.MarkSatisfied(ctx, c.ID, now); err != nil {
		return domain.User{}, challengeErr("satisfy challenge", err)
	}
	if _, err := p.Challenges.Consume(ctx, hash[:], now); err != nil {
		return domain.User{}, challengeErr("consume challenge", err)
	}
	user.MustChangePassword = false
	return user, nil
}

func challengeErr(op string, err error) error {
	if errors.Is(err, port.ErrAuthTokenInvalid) {
		return port.NewActionError(port.ActionErrorInvalid, ErrChallengeInvalid.Error())
	}
	return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("%s: %v", op, err))
}
