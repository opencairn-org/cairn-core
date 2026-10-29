package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"
)

// WebhookSigningToken is the admin view of one external account's webhook
// signing secret: only its rotation time is visible, never the token.
type WebhookSigningToken struct {
	ExternalAccountID ExternalAccountID
	UserID            UserID
	Provider          string
	// RotatedAt is nil for an account that never received a token.
	RotatedAt *time.Time
}

// NewWebhookSigningToken mints a random token and the sha256 we store.
func NewWebhookSigningToken() (plain string, hash []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	plain = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(plain))
	return plain, h[:]
}

// WebhookRotationSelector picks the accounts a bulk rotation touches; the
// set selectors compose with AND.
type WebhookRotationSelector struct {
	Provider   string
	OlderThan  *time.Duration
	AccountIDs []ExternalAccountID
}

var ErrEmptyRotationSelector = errors.New("at least one selector is required")

// Validate refuses an empty selector, which would rotate every token.
func (s WebhookRotationSelector) Validate() error {
	if s.Provider == "" && s.OlderThan == nil && len(s.AccountIDs) == 0 {
		return ErrEmptyRotationSelector
	}
	if s.OlderThan != nil && *s.OlderThan < 0 {
		return errors.New("older_than must not be negative")
	}
	return nil
}
