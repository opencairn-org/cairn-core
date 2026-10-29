package domain

import (
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// OAuth 2.0 device authorization (RFC 8628)
//
// A device that cannot open a browser (or whose keyboard is a remote) asks for
// a code pair: the long device_code it polls with, and a short user_code the
// person types into an already signed-in client (the web app, the phone app)
// that then approves or denies the request. Only the device code's hash is
// stored; the user code is short-lived, single-use and unique among live codes.
// ---------------------------------------------------------------------------

// GrantTypeDeviceCode is the RFC 8628 grant_type at the token endpoint.
const GrantTypeDeviceCode = "urn:ietf:params:oauth:grant-type:device_code"

// UserCodeLength is the number of digits in a user code. Eight digits give
// ~26.6 bits, above the 20 bits RFC 8628 §6.1 asks for, and the verification
// endpoints are rate-limited per client address.
const UserCodeLength = 8

// Scope family for approving sign-ins of other devices. A read looks a pending
// request up, a write decides it. Not part of ReadOnlyScopes: a token gets it
// only when the client asked and the user consented.
const (
	ScopeDevicesRead  = "devices:read"
	ScopeDevicesWrite = "devices:write"
)

// DeviceCodeStatus is where a device authorization stands.
type DeviceCodeStatus string

const (
	DeviceCodePending  DeviceCodeStatus = "pending"
	DeviceCodeApproved DeviceCodeStatus = "approved"
	DeviceCodeDenied   DeviceCodeStatus = "denied"
)

// OAuthDeviceCode is one device authorization request.
type OAuthDeviceCode struct {
	ClientID string
	// UserCode is the normalised form (digits only); Display formats it.
	UserCode string
	// Scope is what the device asked for, filtered to the client's scopes.
	Scope  string
	Status DeviceCodeStatus
	// UserID and GrantedScope are set by the approval.
	UserID       *UserID
	GrantedScope string
	ExpiresAt    time.Time
	DecidedAt    *time.Time
	// ConsumedAt is set when the approval has been exchanged for tokens.
	ConsumedAt *time.Time
	// LastPolledAt is the previous poll, for the slow_down rule.
	LastPolledAt *time.Time
	CreatedAt    time.Time
}

// IsLiveAt reports whether the request can still be decided or polled.
func (d OAuthDeviceCode) IsLiveAt(now time.Time) bool {
	return d.ConsumedAt == nil && now.Before(d.ExpiresAt)
}

// ErrUserCodeFormat means the typed code is not an 8-digit code.
var ErrUserCodeFormat = errors.New("a device code has 8 digits")

// NewUserCode draws a random user code (digits only).
func NewUserCode() (string, error) {
	var b strings.Builder
	for range UserCodeLength {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b.WriteString(n.String())
	}
	return b.String(), nil
}

// NormalizeUserCode accepts what a person typed ("1234-5678", "1234 5678")
// and returns the canonical digits, or ErrUserCodeFormat.
func NormalizeUserCode(input string) (string, error) {
	var b strings.Builder
	for _, r := range input {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == ' ' || r == '\t':
		default:
			return "", ErrUserCodeFormat
		}
	}
	if b.Len() != UserCodeLength {
		return "", ErrUserCodeFormat
	}
	return b.String(), nil
}

// DisplayUserCode renders a canonical code for people: "1234-5678".
func DisplayUserCode(code string) string {
	if len(code) != UserCodeLength {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// CapScopes keeps only the requested scopes an approver may grant. A nil
// approver set means an unrestricted credential (cookie session, PAT).
func CapScopes(requested, approver []string) []string {
	if approver == nil {
		return requested
	}
	allowed := map[string]bool{}
	for _, s := range approver {
		allowed[s] = true
	}
	var out []string
	for _, s := range requested {
		if allowed[s] {
			out = append(out, s)
		}
	}
	return out
}

// ScopeDescription is the consent-page wording of a scope.
func ScopeDescription(scope string) string {
	switch scope {
	case ScopeActivitiesRead:
		return "Read your activities, streams, best-efforts and records"
	case ScopeActivitiesWrite:
		return "Create, edit and delete your activities"
	case ScopeProfileRead:
		return "Read your profile and preferences"
	case ScopeProfileWrite:
		return "Edit your profile and preferences"
	case ScopeSocialRead:
		return "Read your feed, kudos and comments"
	case ScopeSocialWrite:
		return "Post kudos and comments on your behalf"
	case ScopeSegmentsRead:
		return "Read segments and leaderboards"
	case ScopeSegmentsWrite:
		return "Create and edit segments"
	case ScopeTrainingRead:
		return "Read your training-load metrics"
	case ScopeTrainingWrite:
		return "Edit your training and health metrics"
	case ScopeGearRead:
		return "Read your gear"
	case ScopeGearWrite:
		return "Edit your gear"
	case ScopeNotificationsRead:
		return "Read your notifications"
	case ScopeNotificationsWrite:
		return "Manage your notifications and webhooks"
	case ScopeConnectionsRead:
		return "Read your provider connections"
	case ScopeConnectionsWrite:
		return "Manage your provider connections"
	case ScopeDevicesRead:
		return "See sign-in requests from your other devices"
	case ScopeDevicesWrite:
		return "Approve sign-in requests from your other devices"
	default:
		return scope
	}
}
