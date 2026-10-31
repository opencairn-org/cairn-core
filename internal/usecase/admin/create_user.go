// Package admin holds the use cases that drive `cairn admin ...` and (later)
// the AdminService Connect handlers. Each use case takes a small input
// struct, runs validation that can't live in domain alone (uniqueness),
// and persists via the port interfaces.
package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/auth"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// CreateUser creates one user record. Used by the bootstrap flow and by
// the `cairn admin create-user` CLI subcommand. When an OIDC user signs in
// for the first time and auto-provision is enabled, a thin variant of this
// is invoked from the OIDC callback handler (different inputs, same repo).
type CreateUser struct {
	users  port.UserLifecycleStore
	hasher *auth.PasswordHasher

	now   func() time.Time
	newID func() uuid.UUID
}

// NewCreateUser wires the use case. now and newID default to time.Now and
// uuid.NewV7 when nil — overridable for deterministic tests.
func NewCreateUser(
	users port.UserLifecycleStore,
	hasher *auth.PasswordHasher,
	now func() time.Time,
	newID func() uuid.UUID,
) *CreateUser {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if newID == nil {
		newID = func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New()
			}
			return id
		}
	}
	return &CreateUser{users: users, hasher: hasher, now: now, newID: newID}
}

// CreateUserInput is the full set of fields the caller may set. The use
// case normalises, validates, and fills defaults before insert.
type CreateUserInput struct {
	Username    string
	Email       string
	DisplayName string

	// Password is the plaintext password. Set to empty for an OIDC-only or
	// passkey-only account; the user can attach a password later via the
	// account settings.
	Password string

	// MustChangePassword forces a password change on first login. Used by
	// `cairn admin create-user` when the admin types the initial password,
	// so the new user replaces it immediately.
	MustChangePassword bool

	// Role defaults to UserRoleUser. Pass UserRoleAdmin for an admin.
	Role domain.UserRole

	// Optional fields — defaults apply when empty.
	Locale   string
	Timezone string
	Units    domain.UserUnits

	// AvatarURL is preserved as-is. The caller is responsible for any
	// pre-processing (uploading to S3, resizing, etc.) before reaching us.
	AvatarURL string

	// EmailVerified short-circuits the verification email flow. Admin-
	// created accounts default to verified; user self-signups don't.
	EmailVerified bool
}

// Result of CreateUser. The full User is returned so the CLI can print
// the assigned ID and the admin can hand it off to the user.
type CreateUserResult struct {
	User domain.User
}

// Execute validates input, hashes the password (if any), and inserts.
//
// Errors:
//   - domain.ErrUsername* / ErrEmail* / ErrDisplayName* for shape problems
//   - port.ErrUsernameTaken / port.ErrEmailTaken on unique violation
//   - any wrapped repo error otherwise
func (uc *CreateUser) Execute(ctx context.Context, in CreateUserInput) (CreateUserResult, error) {
	username, err := domain.NormaliseUsername(in.Username)
	if err != nil {
		return CreateUserResult{}, err
	}
	email, err := domain.NormaliseEmail(in.Email)
	if err != nil {
		return CreateUserResult{}, err
	}

	role := in.Role
	if role == "" {
		role = domain.UserRoleUser
	}
	if !role.Valid() {
		return CreateUserResult{}, domain.ErrInvalidUserRole
	}

	units := in.Units
	if units == "" {
		units = domain.UserUnitsMetric
	}

	u := domain.User{
		ID:                 domain.UserID(uc.newID()),
		Username:           username,
		Email:              email,
		EmailVerified:      in.EmailVerified,
		DisplayName:        in.DisplayName,
		AvatarURL:          in.AvatarURL,
		Locale:             defaultIfEmpty(in.Locale, "en"),
		Timezone:           defaultIfEmpty(in.Timezone, "UTC"),
		Units:              units,
		Role:               role,
		Status:             domain.UserStatusActive,
		MustChangePassword: in.MustChangePassword,
		CreatedAt:          uc.now(),
		UpdatedAt:          uc.now(),
	}

	if err := domain.ValidateUserShape(u); err != nil {
		return CreateUserResult{}, err
	}

	cred, err := uc.createUserCredentials(u.ID, in.Password)
	if err != nil {
		return CreateUserResult{}, err
	}

	if err := uc.users.CreateUser(ctx, u, cred); err != nil {
		// Surface taken-username/email cleanly to the caller; everything
		// else bubbles up wrapped.
		if errors.Is(err, port.ErrUsernameTaken) || errors.Is(err, port.ErrEmailTaken) {
			return CreateUserResult{}, err
		}
		return CreateUserResult{}, fmt.Errorf("persist user: %w", err)
	}

	return CreateUserResult{User: u}, nil
}

// createUserCredentials hashes the password when one is given.
func (uc *CreateUser) createUserCredentials(id domain.UserID, password string) (domain.Credentials, error) {
	var cred domain.Credentials
	cred.UserID = id
	if password != "" {
		hash, err := uc.hasher.Hash(password)
		if err != nil {
			return domain.Credentials{}, fmt.Errorf("hash password: %w", err)
		}
		cred.PasswordHash = hash
	}
	return cred, nil
}

func defaultIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
