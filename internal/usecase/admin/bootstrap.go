package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// BootstrapAdmin idempotently ensures the instance has at least one admin
// account. Intended to be called once at server start (`cairn serve`) with
// the values from CAIRN_INSTANCE_BOOTSTRAP_ADMIN_EMAIL / _PASSWORD.
//
// Decision matrix:
//
//	state                              action
//	-----                              ------
//	any active admin exists            do nothing (logged at info)
//	no admin AND no bootstrap input    do nothing (logged at warn so the
//	                                   operator notices the gap)
//	no admin AND bootstrap input set   create the admin
//	bootstrap email matches an
//	  existing non-admin user          refuse (never silently promote — too
//	                                   easy to misuse for privilege esc.)
//
// All branches are safe to call repeatedly; multi-replica boot ordering
// is handled by the underlying unique-constraint on email.
type BootstrapAdmin struct {
	users      port.UserReader
	createUser *CreateUser
}

func NewBootstrapAdmin(users port.UserReader, createUser *CreateUser) *BootstrapAdmin {
	return &BootstrapAdmin{users: users, createUser: createUser}
}

// BootstrapAdminInput carries the env-provided initial admin credentials.
// Username, when empty, is derived from the email's local part.
type BootstrapAdminInput struct {
	Email    string
	Password string
	Username string

	// DisplayName defaults to the local part of the email if empty.
	DisplayName string
}

// BootstrapAdminAction reports which branch the use case took. Surfaced
// to logs so operators can verify the boot ran as expected.
type BootstrapAdminAction string

const (
	BootstrapAdminAlreadyPresent BootstrapAdminAction = "already_present"
	BootstrapAdminMissingInput   BootstrapAdminAction = "missing_input"
	BootstrapAdminCreated        BootstrapAdminAction = "created"
)

// BootstrapAdminResult describes the outcome.
type BootstrapAdminResult struct {
	Action BootstrapAdminAction

	// CreatedUser is set only when Action == Created.
	CreatedUser domain.User

	// AdminCount is the post-execution count of active admins. Useful for
	// the startup log line.
	AdminCount int
}

// Execute runs the use case.
func (uc *BootstrapAdmin) Execute(ctx context.Context, in BootstrapAdminInput) (BootstrapAdminResult, error) {
	n, err := uc.users.CountAdmins(ctx)
	if err != nil {
		return BootstrapAdminResult{}, fmt.Errorf("count admins: %w", err)
	}
	if n > 0 {
		return BootstrapAdminResult{Action: BootstrapAdminAlreadyPresent, AdminCount: n}, nil
	}

	if in.Email == "" || in.Password == "" {
		return BootstrapAdminResult{Action: BootstrapAdminMissingInput, AdminCount: 0}, nil
	}

	email, err := domain.NormaliseEmail(in.Email)
	if err != nil {
		return BootstrapAdminResult{}, fmt.Errorf("bootstrap admin: %w", err)
	}

	// If a non-admin user already exists at this email, refuse — promoting
	// silently from env is a privilege-escalation footgun. The operator can
	// instead run `cairn admin promote <userid>`.
	if existing, err := uc.users.GetUserByEmail(ctx, email); err == nil {
		return BootstrapAdminResult{}, fmt.Errorf(
			"bootstrap admin: a non-admin user with email %q already exists (id=%s); "+
				"refusing to silently promote — use `cairn admin promote %s` instead",
			email, existing.ID, existing.ID,
		)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return BootstrapAdminResult{}, fmt.Errorf("bootstrap admin: lookup existing: %w", err)
	}

	username := in.Username
	if username == "" {
		username = deriveUsername(email)
	}

	displayName := in.DisplayName
	if displayName == "" {
		displayName = deriveDisplayName(email)
	}

	res, err := uc.createUser.Execute(ctx, CreateUserInput{
		Username:           username,
		Email:              email,
		DisplayName:        displayName,
		Password:           in.Password,
		MustChangePassword: true, // operator typed it; user replaces on first login
		Role:               domain.UserRoleAdmin,
		EmailVerified:      true, // env-provided value; we trust the operator
	})
	if err != nil {
		return BootstrapAdminResult{}, fmt.Errorf("bootstrap admin: create: %w", err)
	}

	return BootstrapAdminResult{
		Action:      BootstrapAdminCreated,
		CreatedUser: res.User,
		AdminCount:  1,
	}, nil
}

// deriveUsername extracts a safe username from an email's local part. If
// the result fails domain.NormaliseUsername (e.g. very short, or contains
// '@' from a freak input), we fall back to "admin".
func deriveUsername(email string) string {
	local := ""
	for i := 0; i < len(email); i++ {
		if email[i] == '@' {
			local = email[:i]
			break
		}
	}
	if local == "" {
		return "admin"
	}
	cleaned, err := domain.NormaliseUsername(local)
	if err != nil {
		return "admin"
	}
	return cleaned
}

func deriveDisplayName(email string) string {
	for i := 0; i < len(email); i++ {
		if email[i] == '@' {
			return email[:i]
		}
	}
	return email
}
