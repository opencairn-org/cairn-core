package domain

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/language"
)

// ---------------------------------------------------------------------------
// User aggregate
// ---------------------------------------------------------------------------

// User mirrors the users table row. Sensitive fields (the password hash)
// live on Credentials, which is loaded separately to avoid threading
// crypto material through code paths that don't need it.
type User struct {
	ID            UserID
	Username      string // citext; the canonical form is lowercased
	Email         string // citext; the canonical form is lowercased
	EmailVerified bool
	DisplayName   string
	AvatarURL     string

	Locale   string
	Timezone string
	Units    UserUnits

	// Display-format preferences. Empty = follow Locale's default.
	DateFormat string // "" | "iso" | "us" | "eu"
	TimeFormat string // "" | "24h" | "12h"

	Role         UserRole
	Status       UserStatus
	StatusReason string

	MustChangePassword bool

	// PasskeySecondFactor requires a passkey assertion after a password
	// sign-in (the user opts in; needs at least one passkey).
	PasskeySecondFactor bool

	// ProfileIsPublic opts the user's athlete profile into session-less
	// rendering at /u/{username} (multi-user v1, see docs/visibility-model.md).
	ProfileIsPublic bool

	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastLoginAt *time.Time
}

// Credentials holds the user's encoded Argon2id hash. Kept separate from
// User so most code paths never load it (and never accidentally serialise
// it to a response). The hash format is the standard encoded string:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt-b64>$<key-b64>
//
// An empty PasswordHash means the user has no password — sign-in is then
// only possible via OIDC or passkey.
type Credentials struct {
	UserID       UserID
	PasswordHash string
}

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

// UserRole — the roles in the system. Admins manage the whole instance;
// moderators have delegated access to the moderation queue only; normal
// users can only touch their own data.
type UserRole string

const (
	UserRoleUser      UserRole = "user"
	UserRoleModerator UserRole = "moderator"
	UserRoleAdmin     UserRole = "admin"
)

func (r UserRole) Valid() bool {
	switch r {
	case UserRoleUser, UserRoleModerator, UserRoleAdmin:
		return true
	}
	return false
}

// CanModerate reports whether the role grants access to moderation tooling
// (the report queue, hide-activity). Admins and moderators qualify.
func (r UserRole) CanModerate() bool {
	return r == UserRoleAdmin || r == UserRoleModerator
}

// UserStatus is the lifecycle state. The CHECK constraint on users.status
// enforces this set.
type UserStatus string

const (
	UserStatusActive    UserStatus = "active"
	UserStatusInvited   UserStatus = "invited"   // record exists, hasn't completed signup
	UserStatusSuspended UserStatus = "suspended" // admin disabled the account
	UserStatusDeleted   UserStatus = "deleted"   // soft-deleted; preserves audit history
)

func (s UserStatus) Valid() bool {
	switch s {
	case UserStatusActive, UserStatusInvited, UserStatusSuspended, UserStatusDeleted:
		return true
	}
	return false
}

// UserUnits selects the display-unit system for activity summary fields.
// All storage is metric; this only affects rendering.
type UserUnits string

const (
	UserUnitsMetric   UserUnits = "metric"
	UserUnitsImperial UserUnits = "imperial"
)

func (u UserUnits) Valid() bool {
	switch u {
	case UserUnitsMetric, UserUnitsImperial:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// User-level validation errors. Returned by NormaliseUsername / NormaliseEmail
// / ValidateUserShape. The use-case layer surfaces these to the API.
var (
	ErrUsernameRequired     = errors.New("username required")
	ErrUsernameTooShort     = errors.New("username too short (min 3 chars)")
	ErrUsernameTooLong      = errors.New("username too long (max 32 chars)")
	ErrUsernameInvalidChars = errors.New("username may contain only letters, digits, '-', '_', and '.'")
	ErrEmailRequired        = errors.New("email required")
	ErrEmailInvalid         = errors.New("email invalid")
	ErrDisplayNameRequired  = errors.New("display name required")
	ErrInvalidUserRole      = errors.New("invalid user role")
	ErrInvalidUserStatus    = errors.New("invalid user status")
	ErrPasswordTooShort     = errors.New("password must be at least 8 characters")
	ErrAvatarURLInvalid     = errors.New("avatar url must be an absolute http(s) url")
	ErrTimezoneInvalid      = errors.New("timezone must be an IANA zone name")
	ErrLocaleInvalid        = errors.New("locale must be a BCP-47 tag")
)

// NormaliseUsername lowercases, trims, and validates the username. The
// repository stores usernames in citext so the lookup is case-insensitive;
// we still normalise on input to make %LIKE% and audit-log diffs stable.
func NormaliseUsername(in string) (string, error) {
	u := strings.ToLower(strings.TrimSpace(in))
	switch {
	case u == "":
		return "", ErrUsernameRequired
	case len(u) < 3:
		return "", ErrUsernameTooShort
	case len(u) > 32:
		return "", ErrUsernameTooLong
	}
	for _, r := range u {
		if !isUsernameRune(r) {
			return "", ErrUsernameInvalidChars
		}
	}
	return u, nil
}

func isUsernameRune(r rune) bool {
	switch {
	case unicode.IsLetter(r), unicode.IsDigit(r):
		return true
	case r == '-' || r == '_' || r == '.':
		return true
	}
	return false
}

// NormaliseEmail lowercases, trims, and validates the email address using
// the stdlib mail package (RFC 5322 envelope-conformant — we don't try to
// be cleverer than `mail.ParseAddress`).
func NormaliseEmail(in string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(in))
	if e == "" {
		return "", ErrEmailRequired
	}
	a, err := mail.ParseAddress(e)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrEmailInvalid, err)
	}
	// A display name ("Bob <bob@x>") parses too; only a bare address is an account email.
	if a.Address != e {
		return "", fmt.Errorf("%w: not a bare address", ErrEmailInvalid)
	}
	return e, nil
}

// ValidateUserShape checks fields whose validity does not require I/O
// (username format, role / status enum membership, display name presence).
// Uniqueness of username and email is enforced by the database; the
// use-case layer translates pg unique-violation errors into domain errors.
// MinPasswordChars is the instance-wide password floor (NIST 800-63B: length
// over composition rules).
const MinPasswordChars = 8

// ValidatePassword applies the password policy to a plaintext candidate.
func ValidatePassword(pw string) error {
	if len(pw) < MinPasswordChars {
		return ErrPasswordTooShort
	}
	return nil
}

// ValidateAvatarURL accepts an empty value or an absolute http(s) URL.
func ValidateAvatarURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrAvatarURLInvalid
	}
	return nil
}

// ValidateTimezone accepts an empty value or a loadable IANA zone.
func ValidateTimezone(tz string) error {
	if tz == "" {
		return nil
	}
	if _, err := time.LoadLocation(tz); err != nil || tz == "Local" {
		return ErrTimezoneInvalid
	}
	return nil
}

// ValidateLocale accepts an empty value or a well-formed BCP-47 tag.
func ValidateLocale(tag string) error {
	if tag == "" {
		return nil
	}
	if _, err := language.Parse(tag); err != nil {
		return ErrLocaleInvalid
	}
	return nil
}

func ValidateUserShape(u User) error {
	if _, err := NormaliseUsername(u.Username); err != nil {
		return err
	}
	if _, err := NormaliseEmail(u.Email); err != nil {
		return err
	}
	if strings.TrimSpace(u.DisplayName) == "" {
		return ErrDisplayNameRequired
	}
	if !u.Role.Valid() {
		return ErrInvalidUserRole
	}
	if !u.Status.Valid() {
		return ErrInvalidUserStatus
	}
	if u.Units != "" && !u.Units.Valid() {
		return fmt.Errorf("invalid units %q", u.Units)
	}
	return nil
}

// IsActive reports whether the user can sign in. Used by the auth middleware
// gate. Note: this is intentionally narrow — invited users CANNOT sign in
// with a password until they redeem their invite.
func (u User) IsActive() bool {
	return u.Status == UserStatusActive
}
