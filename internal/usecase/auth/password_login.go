package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Password sign-in refusals; any other error is a server fault.
var (
	ErrTooManyAttempts    = errors.New("too many failed sign-in attempts")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrAccountInactive    = errors.New("account is not active")
)

// PasswordLogin checks a username-or-email + password pair behind the login guard.
type PasswordLogin struct {
	Users  port.UserRepo
	Hasher interface {
		Verify(password, encoded string) (bool, error)
	}
	Guard *LoginGuard
}

// Login returns the user the credentials belong to; address feeds the guard.
func (p *PasswordLogin) Login(ctx context.Context, identifier, password, address string) (domain.User, error) {
	if !p.Guard.Allow(identifier, address) {
		return domain.User{}, ErrTooManyAttempts
	}
	user, err := p.verify(ctx, identifier, password)
	if errors.Is(err, ErrInvalidCredentials) || errors.Is(err, ErrAccountInactive) {
		p.Guard.Failure(ctx, identifier, address)
	}
	if err != nil {
		return domain.User{}, err
	}
	p.Guard.Success(identifier)
	return user, nil
}

func (p *PasswordLogin) verify(ctx context.Context, identifier, password string) (domain.User, error) {
	user, cred, err := p.Users.GetCredentialsByLogin(ctx, identifier)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Burn an Argon2id round so present and absent users take equal time.
			_, _ = p.Hasher.Verify(password, "")
			return domain.User{}, ErrInvalidCredentials
		}
		return domain.User{}, fmt.Errorf("credentials lookup: %w", err)
	}
	ok, err := p.Hasher.Verify(password, cred.PasswordHash)
	if err != nil {
		return domain.User{}, fmt.Errorf("verify password of %s: %w", user.ID, err)
	}
	if !ok {
		return domain.User{}, ErrInvalidCredentials
	}
	if !user.IsActive() {
		return domain.User{}, ErrAccountInactive
	}
	return user, nil
}
