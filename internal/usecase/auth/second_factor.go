package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// SecondFactor runs the passkey-after-password step: a password sign-in of
// an opted-in user yields a challenge instead of a session; a passkey
// assertion by the same user satisfies it; completing a satisfied challenge
// hands back the user id so the transport can issue the session.
type SecondFactor struct {
	Challenges port.LoginChallengeRepo
	Users      port.UserRepo
	Passkeys   port.PasskeyRepo
	Now        func() time.Time
}

// ErrChallengeInvalid is the user-facing reason for a bad, stale or
// mismatched challenge.
var ErrChallengeInvalid = errors.New("this sign-in challenge is invalid or has expired")

func (s *SecondFactor) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// Required reports whether the user's password sign-in needs a passkey.
func (s *SecondFactor) Required(u domain.User) bool {
	return s != nil && s.Passkeys != nil && u.PasskeySecondFactor
}

// Begin stores a challenge for the user and returns its one-time token.
func (s *SecondFactor) Begin(ctx context.Context, userID domain.UserID) (string, error) {
	return beginChallenge(ctx, s.Challenges, userID, domain.ChallengeSecondFactor, s.now())
}

// beginChallenge mints a one-time token and stores its hash with the purpose.
func beginChallenge(ctx context.Context, repo port.LoginChallengeRepo, userID domain.UserID, purpose domain.LoginChallengePurpose, now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("challenge token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	if _, err := repo.Create(ctx, domain.LoginChallenge{
		UserID: userID, Purpose: purpose, TokenHash: hash[:], CreatedAt: now, ExpiresAt: now.Add(domain.LoginChallengeTTL),
	}); err != nil {
		return "", fmt.Errorf("store challenge: %w", err)
	}
	return token, nil
}

// secondFactorChallenge loads a challenge of the passkey purpose.
func (s *SecondFactor) secondFactorChallenge(ctx context.Context, token string) (domain.LoginChallenge, error) {
	hash := sha256.Sum256([]byte(token))
	c, err := s.Challenges.GetByTokenHash(ctx, hash[:])
	if err != nil || (c.Purpose != "" && c.Purpose != domain.ChallengeSecondFactor) {
		return domain.LoginChallenge{}, port.NewActionError(port.ActionErrorInvalid, ErrChallengeInvalid.Error())
	}
	return c, nil
}

// Satisfy records a verified passkey assertion of userID against the
// challenge; an assertion by another user is refused.
func (s *SecondFactor) Satisfy(ctx context.Context, token string, userID domain.UserID) error {
	c, err := s.secondFactorChallenge(ctx, token)
	if err != nil || c.UserID != userID || c.ConsumedAt != nil || !s.now().Before(c.ExpiresAt) {
		return port.NewActionError(port.ActionErrorInvalid, ErrChallengeInvalid.Error())
	}
	if err := s.Challenges.MarkSatisfied(ctx, c.ID, s.now()); err != nil {
		if errors.Is(err, port.ErrAuthTokenInvalid) {
			return port.NewActionError(port.ActionErrorInvalid, ErrChallengeInvalid.Error())
		}
		return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("satisfy challenge: %v", err))
	}
	return nil
}

// Complete consumes a satisfied challenge and returns the user to sign in.
func (s *SecondFactor) Complete(ctx context.Context, token string) (domain.UserID, error) {
	if _, err := s.secondFactorChallenge(ctx, token); err != nil {
		return domain.UserID{}, err
	}
	hash := sha256.Sum256([]byte(token))
	uid, err := s.Challenges.Consume(ctx, hash[:], s.now())
	if errors.Is(err, port.ErrAuthTokenInvalid) {
		return domain.UserID{}, port.NewActionError(port.ActionErrorInvalid, ErrChallengeInvalid.Error())
	}
	if err != nil {
		return domain.UserID{}, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("complete challenge: %v", err))
	}
	return uid, nil
}

// SetEnabled turns the second factor on (needs a passkey) or off.
func (s *SecondFactor) SetEnabled(ctx context.Context, userID domain.UserID, enabled bool) error {
	if enabled {
		if s.Passkeys == nil {
			return port.NewActionError(port.ActionErrorUnavailable, "passkeys are not available on this instance")
		}
		keys, err := s.Passkeys.ListByUser(ctx, userID)
		if err != nil {
			return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("list passkeys: %v", err))
		}
		if len(keys) == 0 {
			return port.NewActionError(port.ActionErrorPrecondition, "add a passkey before requiring it as a second factor")
		}
	}
	if err := s.Users.SetPasskeySecondFactor(ctx, userID, enabled); err != nil {
		return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("set second factor: %v", err))
	}
	return nil
}
