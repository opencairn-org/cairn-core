package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// UserSettingsRepo reads (and later updates) UserSettings. The merge
// engine's PolicyResolver is typically backed by GetMergePolicy.
//
// The full UserSettings structure is broader than what this port exposes;
// methods are added as use cases need them.
type UserSettingsRepo interface {
	// GetMergePolicy returns the user's configured policy for the named
	// activity type. Implementations fall back to
	// domain.DefaultMergePolicyFor(t) when no override exists; callers
	// therefore never receive a zero policy.
	GetMergePolicy(
		ctx context.Context,
		userID domain.UserID,
		activityType domain.ActivityType,
	) (domain.MergePolicy, error)

	// GetAllMergePolicies returns every per-activity-type policy the user has
	// explicitly configured (empty map when none). For the settings editor.
	GetAllMergePolicies(ctx context.Context, userID domain.UserID) (map[domain.ActivityType]domain.MergePolicy, error)

	// SetMergePolicies replaces the user's full per-activity-type policy map.
	// An empty map clears all user overrides (falls back to instance defaults).
	SetMergePolicies(ctx context.Context, userID domain.UserID, policies map[domain.ActivityType]domain.MergePolicy) error

	// GetInstanceMergeDefaults / SetInstanceMergeDefaults manage the instance-wide
	// default policies (the cascade level below per-user, above the trivial
	// AnyProvider fallback).
	GetInstanceMergeDefaults(ctx context.Context) (map[domain.ActivityType]domain.MergePolicy, error)
	SetInstanceMergeDefaults(ctx context.Context, policies map[domain.ActivityType]domain.MergePolicy) error
}

// ProfileUpdate carries the columns UpdateProfile writes; every field is
// written as given (the use case fills unchanged ones from the current row).
type ProfileUpdate struct {
	DisplayName string
	AvatarURL   string
	Locale      string
	Timezone    string
	Units       string
}

// UserReader looks up and lists users.
type UserReader interface {
	// GetUser reads one users row by ID. Returns domain.ErrNotFound on miss.
	GetUser(ctx context.Context, id domain.UserID) (domain.User, error)

	// ListAdmins returns every active admin user (recipients for instance-level
	// notifications like worker-offline).
	ListAdmins(ctx context.Context) ([]domain.User, error)

	// GetUserByUsername resolves the canonical lowercase username to a User.
	// Returns domain.ErrNotFound on miss.
	GetUserByUsername(ctx context.Context, username string) (domain.User, error)

	// GetUserByEmail resolves the canonical lowercase email to a User.
	// Returns domain.ErrNotFound on miss.
	GetUserByEmail(ctx context.Context, email string) (domain.User, error)

	// CountAdmins returns the number of users with role=admin and
	// status=active. Used by:
	//   - admin demote/suspend: refuse to remove the last admin
	//   - bootstrap: detect whether the instance already has any admin
	CountAdmins(ctx context.Context) (int, error)

	// ListUsers returns users ordered by created_at descending. Soft-deleted
	// rows are excluded. Pagination is keyset-based via the cursor returned
	// alongside the page (empty when no more rows).
	ListUsers(ctx context.Context, page UserListPage) ([]domain.User, string, error)
}

// UserCredentialStore reads and updates login credentials and second-factor settings.
type UserCredentialStore interface {
	// GetCredentialsByLogin resolves a login (which may be either a username
	// or an email) and returns the matching User plus its credentials. The
	// password hash on the returned Credentials may be empty if the user
	// has not set a password (OIDC-only / passkey-only account).
	//
	// The combined lookup avoids exposing whether a non-existent user has
	// a password or not — the auth use case always Argon2id-verifies, even
	// for missing users, to keep response time uniform.
	GetCredentialsByLogin(ctx context.Context, login string) (domain.User, domain.Credentials, error)

	// UpdatePassword sets the encoded Argon2id hash and clears
	// users.must_change_password. Callers in the password-reset flow pass
	// an empty hash to disable password login.
	UpdatePassword(ctx context.Context, id domain.UserID, encodedHash string) error

	// SetMustChangePassword flags (or clears) the forced change on next login.
	SetMustChangePassword(ctx context.Context, id domain.UserID, must bool) error

	// SetPasskeySecondFactor sets users.passkey_second_factor.
	SetPasskeySecondFactor(ctx context.Context, id domain.UserID, enabled bool) error
}

// UserProfileWriter updates self-editable profile, preference and email columns.
type UserProfileWriter interface {
	// SetProfilePublic toggles whether the user's profile renders to
	// anonymous viewers at /u/{username} (multi-user v1).
	SetProfilePublic(ctx context.Context, id domain.UserID, public bool) error

	// UpdateUserPreferences sets display-format preferences (units + date/time
	// format). Empty strings reset to the locale default.
	UpdateUserPreferences(ctx context.Context, id domain.UserID, units, dateFormat, timeFormat string) error

	// UpdateProfile sets the self-editable profile columns in one statement.
	UpdateProfile(ctx context.Context, id domain.UserID, p ProfileUpdate) error

	// UpdateEmail sets the address (and its verified flag); ErrEmailTaken on a clash.
	UpdateEmail(ctx context.Context, id domain.UserID, email string, verified bool) error

	// UpdateEmailVerified sets users.email_verified.
	UpdateEmailVerified(ctx context.Context, id domain.UserID, verified bool) error
}

// UserFederationStore manages the per-user ActivityPub opt-in.
type UserFederationStore interface {
	// SetFederationEnabled / IsFederationEnabled manage the per-user ActivityPub
	// opt-in (docs/federation-design.md). Off by default.
	SetFederationEnabled(ctx context.Context, id domain.UserID, enabled bool) error
	IsFederationEnabled(ctx context.Context, id domain.UserID) (bool, error)
}

// UserLifecycleStore creates and deletes users and changes their role or status.
type UserLifecycleStore interface {
	// CreateUser inserts a new users row (and an associated credentials
	// row when cred.PasswordHash is non-empty). The provided User must
	// already pass domain.ValidateUserShape. Returns ErrUsernameTaken or
	// ErrEmailTaken when the database's unique constraint trips.
	CreateUser(ctx context.Context, u domain.User, cred domain.Credentials) error

	// UpdateUserRole sets users.role. Used by `cairn admin promote/demote`.
	UpdateUserRole(ctx context.Context, id domain.UserID, role domain.UserRole) error

	// UpdateUserStatus sets users.status and users.status_reason. Used by
	// `cairn admin suspend/reactivate`.
	UpdateUserStatus(
		ctx context.Context,
		id domain.UserID,
		status domain.UserStatus,
		reason string,
	) error

	// HardDeleteUser removes the users row; dependent rows cascade.
	HardDeleteUser(ctx context.Context, id domain.UserID) error
}

// UserProfileStore reads users and updates their self-editable profile.
type UserProfileStore interface {
	UserReader
	UserProfileWriter
}

// UserLoginStore reads users and their login credentials.
type UserLoginStore interface {
	UserReader
	UserCredentialStore
}

// UserRepo holds users and their credentials.
//
// Credentials live on a separate read path (GetCredentialsByLogin) so
// password hashes are not loaded into request handlers that only need
// the public User shape.
type UserRepo interface {
	UserReader
	UserCredentialStore
	UserProfileWriter
	UserFederationStore
	UserLifecycleStore
}

// UserListPage paginates ListUsers via keyset. Cursor is the opaque token
// returned by the previous call; PageSize defaults to 50 when zero.
type UserListPage struct {
	Cursor   string
	PageSize int
}

// Errors specific to UserRepo. Repository implementations wrap them with
// fmt.Errorf("...: %w", err); use cases match with errors.Is.
var (
	ErrUsernameTaken = repoError("username already taken")
	ErrEmailTaken    = repoError("email already taken")
	ErrLastAdmin     = repoError("refusing to leave the instance without an active admin")
)

// repoError is a tiny sentinel type. We can't depend on domain here for the
// error type (would create an import cycle if domain ever depends on port).
type repoError string

func (e repoError) Error() string { return string(e) }
