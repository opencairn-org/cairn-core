package domain

import "time"

// LoginChallengeTTL bounds the time between a password sign-in and the
// passkey assertion that completes it.
const LoginChallengeTTL = 5 * time.Minute

// SessionTokenPrefix marks a bearer session token issued to a non-browser
// client (Complete2FA); cookie sessions carry no prefix.
const SessionTokenPrefix = "cairn_ses_" //nolint:gosec // G101: token prefix, not a credential

// LoginChallengePurpose says what a challenge waits for.
type LoginChallengePurpose string

const (
	// ChallengeSecondFactor waits for a passkey assertion.
	ChallengeSecondFactor LoginChallengePurpose = "second_factor"
	// ChallengePasswordChange waits for the replacement of a temporary password.
	ChallengePasswordChange LoginChallengePurpose = "password_change"
)

// LoginChallenge is a password sign-in waiting for one more step.
type LoginChallenge struct {
	ID          LoginChallengeID
	UserID      UserID
	Purpose     LoginChallengePurpose
	TokenHash   []byte
	CreatedAt   time.Time
	ExpiresAt   time.Time
	SatisfiedAt *time.Time
	ConsumedAt  *time.Time
}

// Completable reports whether the challenge can turn into a session now.
func (c LoginChallenge) Completable(now time.Time) bool {
	return c.SatisfiedAt != nil && c.ConsumedAt == nil && now.Before(c.ExpiresAt)
}

// Open reports whether the challenge still accepts its step.
func (c LoginChallenge) Open(now time.Time) bool {
	return c.SatisfiedAt == nil && c.ConsumedAt == nil && now.Before(c.ExpiresAt)
}
