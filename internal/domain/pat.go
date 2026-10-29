package domain

import (
	"errors"
	"strings"
	"time"
)

// PATTokenPrefix marks a personal access token in an Authorization header.
const PATTokenPrefix = "cairn_pat_" //nolint:gosec // G101: token prefix, not a credential

// PATPrefixVisibleChars is how much of the plaintext after the prefix the
// stored TokenPrefix reveals.
const PATPrefixVisibleChars = 6

// PATDisplayPrefix returns the identify-without-exposing prefix stored for a plaintext token.
func PATDisplayPrefix(token string) string {
	n := len(PATTokenPrefix) + PATPrefixVisibleChars
	if len(token) < n {
		return token
	}
	return token[:n] + "…"
}

// ErrPATNameRequired rejects a token without a name.
var ErrPATNameRequired = errors.New("token name required")

// ValidatePATName trims and checks a token name.
func ValidatePATName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrPATNameRequired
	}
	return name, nil
}

// PersonalAccessToken is a long-lived bearer credential a user mints for the
// CLI / API clients. Only the SHA-256 hash is stored; the plaintext is shown
// once at creation. A PAT authenticates as its owning user (scopes are stored
// for future granular enforcement; today a PAT carries the user's full access).
type PersonalAccessToken struct {
	ID          PATID
	UserID      UserID
	Name        string
	TokenPrefix string // first chars of the plaintext, for identify-without-exposing
	Scopes      []string
	CreatedAt   time.Time
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
	LastUsedAt  *time.Time
}

// IsValidAt reports whether the token can authenticate at `now` (not revoked,
// not expired).
func (p PersonalAccessToken) IsValidAt(now time.Time) bool {
	if p.RevokedAt != nil {
		return false
	}
	if p.ExpiresAt != nil && !now.Before(*p.ExpiresAt) {
		return false
	}
	return true
}
