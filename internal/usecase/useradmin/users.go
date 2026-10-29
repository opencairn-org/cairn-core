// Package useradmin is the admin-side user management (AdminService and
// /api/admin/users). It sits beside usecase/admin because it composes the
// invite use case, which itself builds on admin.CreateUser.
package useradmin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/admin"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
	"github.com/opencairn-org/cairn-core/internal/usecase/invite"
)

// Users is the admin user management behind AdminService and
// /api/admin/users: every action is audited and refuses to leave the
// instance without an active admin.
type Users struct {
	Users            port.UserRepo
	Sessions         port.SessionRepo
	PATs             port.PATRepo
	Passkeys         port.PasskeyRepo
	Identities       port.LinkedIdentityRepo
	Activities       port.ActivityRepo
	ExternalAccounts port.ExternalAccountRepo
	Create           *admin.CreateUser
	Invites          *invite.CreateInvite
	Hasher           interface {
		Hash(password string) (string, error)
	}
	Audit  *audit.Recorder
	Quotas port.QuotaRepo
}

// UserDetail is the admin view of one user.
type UserDetail struct {
	User           domain.User
	Identities     []domain.LinkedIdentity
	ActiveSessions int
	Footprint      port.UserFootprint
}

// UserPatch carries the admin-editable profile fields; nil leaves a field.
type UserPatch struct {
	Email       *string
	DisplayName *string
	Locale      *string
	Timezone    *string
	Units       *string
}

// CreateResult is a created user plus the invite minted when no password was given.
type CreateResult struct {
	User   domain.User
	Invite *invite.CreateInviteResult
}

// CredentialRevocation says what RevokeCredentials removed.
type CredentialRevocation struct {
	Sessions, PATs, Passkeys int
}

// DeleteCounts says what a hard delete removed.
type DeleteCounts struct {
	Activities, ExternalAccounts int
	StorageBytes                 int64
}

func notFound() error { return port.NewActionError(port.ActionErrorNotFound, "user not found") }

func internalErr(op string, err error) error {
	return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("%s: %v", op, err))
}

func invalid(err error) error { return port.NewActionError(port.ActionErrorInvalid, err.Error()) }

// load returns the user or a not-found action error.
func (u *Users) load(ctx context.Context, id domain.UserID) (domain.User, error) {
	user, err := u.Users.GetUser(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.User{}, notFound()
	}
	if err != nil {
		return domain.User{}, internalErr("load user", err)
	}
	return user, nil
}

// Get assembles the admin detail view.
func (u *Users) Get(ctx context.Context, id domain.UserID) (UserDetail, error) {
	user, err := u.load(ctx, id)
	if err != nil {
		return UserDetail{}, err
	}
	d := UserDetail{User: user}
	if u.Identities != nil {
		d.Identities, _ = u.Identities.ListForUser(ctx, id)
	}
	if u.Sessions != nil {
		if list, err := u.Sessions.ListForUser(ctx, id); err == nil {
			d.ActiveSessions = len(list)
		}
	}
	if fps, err := u.Footprints(ctx, []domain.UserID{id}); err == nil {
		d.Footprint = fps[id]
	}
	return d, nil
}

// Footprints returns admin counters for users (empty map without an activity repo).
func (u *Users) Footprints(ctx context.Context, ids []domain.UserID) (map[domain.UserID]port.UserFootprint, error) {
	if u.Activities == nil || len(ids) == 0 {
		return map[domain.UserID]port.UserFootprint{}, nil
	}
	fps, err := u.Activities.UserFootprints(ctx, ids)
	if err != nil {
		return nil, internalErr("user footprints", err)
	}
	return fps, nil
}

// CreateInput is an admin-created account.
type CreateInput struct {
	Username, Email, DisplayName string
	Role                         domain.UserRole
	InitialPassword              string
	Locale, Timezone             string
	Units                        domain.UserUnits
}

// CreateUser creates the account (with a must-change password) or, without
// a password, an invited account plus the invite the admin hands over.
func (u *Users) CreateUser(ctx context.Context, actor audit.Actor, in CreateInput) (CreateResult, error) {
	if u.Create == nil {
		return CreateResult{}, port.NewActionError(port.ActionErrorUnavailable, "user creation is not available")
	}
	role := in.Role
	if role == "" {
		role = domain.UserRoleUser
	}
	if !role.Valid() {
		return CreateResult{}, invalid(domain.ErrInvalidUserRole)
	}
	if in.InitialPassword != "" {
		if err := domain.ValidatePassword(in.InitialPassword); err != nil {
			return CreateResult{}, invalid(err)
		}
	}
	res, err := u.Create.Execute(ctx, admin.CreateUserInput{
		Username: in.Username, Email: in.Email, DisplayName: in.DisplayName, Password: in.InitialPassword,
		MustChangePassword: in.InitialPassword != "", Role: role, Locale: in.Locale, Timezone: in.Timezone, Units: in.Units,
		EmailVerified: true,
	})
	if err != nil {
		if errors.Is(err, port.ErrUsernameTaken) || errors.Is(err, port.ErrEmailTaken) {
			return CreateResult{}, port.NewActionError(port.ActionErrorConflict, err.Error())
		}
		return CreateResult{}, invalid(err)
	}
	out := CreateResult{User: res.User}
	if in.InitialPassword == "" && u.Invites != nil {
		actorID := actor.UserID
		inv, err := u.Invites.Execute(ctx, invite.CreateInviteInput{Role: role, Email: res.User.Email, CreatedBy: actorID})
		if err != nil {
			return out, internalErr("create invite", err)
		}
		out.Invite = &inv
	}
	u.Audit.Info(ctx, actor, domain.AuditUserCreated, "user", res.User.ID.String(), map[string]string{"username": res.User.Username, "role": string(role)})
	return out, nil
}

// Update edits the admin-editable fields; an email change clears verification.
func (u *Users) Update(ctx context.Context, actor audit.Actor, id domain.UserID, p UserPatch) (domain.User, error) {
	user, err := u.load(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	var diff []domain.FieldChange
	if p.Email != nil {
		change, err := u.changeEmail(ctx, user, *p.Email)
		if err != nil {
			return domain.User{}, err
		}
		diff = append(diff, change...)
	}
	upd, err := applyUserPatch(user, p)
	if err != nil {
		return domain.User{}, invalid(err)
	}
	diff = append(diff, profileDiff(user, upd)...)
	if err := u.Users.UpdateProfile(ctx, id, upd); err != nil {
		return domain.User{}, internalErr("update profile", err)
	}
	if len(diff) > 0 {
		u.Audit.Record(ctx, actor, domain.AuditUserUpdated, domain.AuditInfo, "user", id.String(), diff, nil)
	}
	return u.load(ctx, id)
}

// changeEmail stores a new address unverified; unchanged addresses are a no-op.
func (u *Users) changeEmail(ctx context.Context, user domain.User, raw string) ([]domain.FieldChange, error) {
	email, err := domain.NormaliseEmail(raw)
	if err != nil {
		return nil, invalid(err)
	}
	if email == user.Email {
		return nil, nil
	}
	if err := u.Users.UpdateEmail(ctx, user.ID, email, false); err != nil {
		if errors.Is(err, port.ErrEmailTaken) {
			return nil, port.NewActionError(port.ActionErrorConflict, err.Error())
		}
		return nil, internalErr("update email", err)
	}
	return []domain.FieldChange{{Field: "email", Before: user.Email, After: email}}, nil
}

// applyUserPatch lays the profile fields of the patch over the current row.
func applyUserPatch(user domain.User, p UserPatch) (port.ProfileUpdate, error) {
	upd := port.ProfileUpdate{DisplayName: user.DisplayName, AvatarURL: user.AvatarURL, Locale: user.Locale, Timezone: user.Timezone, Units: string(user.Units)}
	if p.DisplayName != nil {
		if upd.DisplayName = strings.TrimSpace(*p.DisplayName); upd.DisplayName == "" {
			return upd, domain.ErrDisplayNameRequired
		}
	}
	if p.Locale != nil {
		upd.Locale = strings.TrimSpace(*p.Locale)
		if err := domain.ValidateLocale(upd.Locale); err != nil {
			return upd, err
		}
	}
	if p.Timezone != nil {
		upd.Timezone = strings.TrimSpace(*p.Timezone)
		if err := domain.ValidateTimezone(upd.Timezone); err != nil {
			return upd, err
		}
	}
	if p.Units != nil {
		if upd.Units = *p.Units; !domain.UserUnits(upd.Units).Valid() {
			return upd, errors.New("units must be metric or imperial")
		}
	}
	return upd, nil
}

func profileDiff(before domain.User, after port.ProfileUpdate) []domain.FieldChange {
	var out []domain.FieldChange
	add := func(f, a, b string) {
		if a != b {
			out = append(out, domain.FieldChange{Field: f, Before: a, After: b})
		}
	}
	add("display_name", before.DisplayName, after.DisplayName)
	add("locale", before.Locale, after.Locale)
	add("timezone", before.Timezone, after.Timezone)
	add("units", string(before.Units), after.Units)
	return out
}

// ChangeRole sets the role, refusing to demote the last active admin.
func (u *Users) ChangeRole(ctx context.Context, actor audit.Actor, id domain.UserID, role domain.UserRole) (domain.User, error) {
	if !role.Valid() {
		return domain.User{}, invalid(domain.ErrInvalidUserRole)
	}
	user, err := u.load(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	if user.Role == domain.UserRoleAdmin && role != domain.UserRoleAdmin {
		if err := u.notLastAdmin(ctx); err != nil {
			return domain.User{}, err
		}
	}
	if err := u.Users.UpdateUserRole(ctx, id, role); err != nil {
		return domain.User{}, internalErr("update role", err)
	}
	u.Audit.Record(ctx, actor, domain.AuditUserRoleChanged, domain.AuditHigh, "user", id.String(),
		[]domain.FieldChange{{Field: "role", Before: string(user.Role), After: string(role)}}, nil)
	return u.load(ctx, id)
}

// SetStatus suspends or reactivates; suspension ends every session.
func (u *Users) SetStatus(ctx context.Context, actor audit.Actor, id domain.UserID, status domain.UserStatus, reason string) (domain.User, error) {
	if status != domain.UserStatusActive && status != domain.UserStatusSuspended {
		return domain.User{}, invalid(errors.New("status must be active or suspended; use delete for deletion"))
	}
	user, err := u.load(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	if actor.UserID != nil && *actor.UserID == id && status == domain.UserStatusSuspended {
		return domain.User{}, port.NewActionError(port.ActionErrorPrecondition, "you cannot suspend yourself")
	}
	if user.Role == domain.UserRoleAdmin && status == domain.UserStatusSuspended {
		if err := u.notLastAdmin(ctx); err != nil {
			return domain.User{}, err
		}
	}
	if err := u.Users.UpdateUserStatus(ctx, id, status, reason); err != nil {
		return domain.User{}, internalErr("update status", err)
	}
	if status == domain.UserStatusSuspended && u.Sessions != nil {
		_ = u.Sessions.RevokeAllForUser(ctx, id)
	}
	u.Audit.Record(ctx, actor, domain.AuditUserStatusChanged, domain.AuditHigh, "user", id.String(),
		[]domain.FieldChange{{Field: "status", Before: string(user.Status), After: string(status)}}, map[string]string{"reason": reason})
	return u.load(ctx, id)
}

// Delete soft-deletes (status=deleted, sessions ended) or hard-deletes the
// user; confirmUsername must match.
func (u *Users) Delete(ctx context.Context, actor audit.Actor, id domain.UserID, confirmUsername string, hard bool) (DeleteCounts, error) {
	user, err := u.load(ctx, id)
	if err != nil {
		return DeleteCounts{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(confirmUsername), user.Username) {
		return DeleteCounts{}, port.NewActionError(port.ActionErrorPrecondition, "confirm_username does not match")
	}
	if actor.UserID != nil && *actor.UserID == id {
		return DeleteCounts{}, port.NewActionError(port.ActionErrorPrecondition, "you cannot delete yourself")
	}
	if user.Role == domain.UserRoleAdmin {
		if err := u.notLastAdmin(ctx); err != nil {
			return DeleteCounts{}, err
		}
	}
	var counts DeleteCounts
	if fps, err := u.Footprints(ctx, []domain.UserID{id}); err == nil {
		fp := fps[id]
		counts = DeleteCounts{Activities: fp.Activities, ExternalAccounts: fp.ExternalAccounts, StorageBytes: fp.StorageBytes}
	}
	if u.Sessions != nil {
		_ = u.Sessions.RevokeAllForUser(ctx, id)
	}
	if hard {
		if err := u.Users.HardDeleteUser(ctx, id); err != nil {
			return DeleteCounts{}, internalErr("hard delete", err)
		}
	} else {
		if err := u.Users.UpdateUserStatus(ctx, id, domain.UserStatusDeleted, "admin_delete"); err != nil {
			return DeleteCounts{}, internalErr("soft delete", err)
		}
		counts = DeleteCounts{}
	}
	u.Audit.High(ctx, actor, domain.AuditUserDeleted, "user", id.String(), map[string]string{"username": user.Username, "hard": fmt.Sprint(hard)})
	return counts, nil
}

// ResetPassword sets a temporary password the user must change and ends
// their sessions; the password is returned once.
func (u *Users) ResetPassword(ctx context.Context, actor audit.Actor, id domain.UserID) (string, error) {
	if u.Hasher == nil {
		return "", port.NewActionError(port.ActionErrorUnavailable, "password hashing is not available")
	}
	if _, err := u.load(ctx, id); err != nil {
		return "", err
	}
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", internalErr("generate password", err)
	}
	temp := base64.RawURLEncoding.EncodeToString(raw)
	encoded, err := u.Hasher.Hash(temp)
	if err != nil {
		return "", internalErr("hash password", err)
	}
	if err := u.Users.UpdatePassword(ctx, id, encoded); err != nil {
		return "", internalErr("update password", err)
	}
	if err := u.Users.SetMustChangePassword(ctx, id, true); err != nil {
		return "", internalErr("flag password change", err)
	}
	if u.Sessions != nil {
		_ = u.Sessions.RevokeAllForUser(ctx, id)
	}
	u.Audit.High(ctx, actor, domain.AuditUserPasswordReset, "user", id.String(), nil)
	return temp, nil
}

// RevokeCredentials ends sessions, revokes PATs and removes passkeys as
// selected; nothing selected means everything.
func (u *Users) RevokeCredentials(ctx context.Context, actor audit.Actor, id domain.UserID, sessions, pats, passkeys bool) (CredentialRevocation, error) {
	if _, err := u.load(ctx, id); err != nil {
		return CredentialRevocation{}, err
	}
	if !sessions && !pats && !passkeys {
		sessions, pats, passkeys = true, true, true
	}
	var out CredentialRevocation
	var err error
	if sessions {
		if out.Sessions, err = u.revokeSessions(ctx, id); err != nil {
			return out, err
		}
	}
	if pats {
		if out.PATs, err = u.revokePATs(ctx, id); err != nil {
			return out, err
		}
	}
	if passkeys {
		if out.Passkeys, err = u.removePasskeys(ctx, id); err != nil {
			return out, err
		}
	}
	u.Audit.High(ctx, actor, domain.AuditUserCredentialsRevoke, "user", id.String(),
		map[string]string{"sessions": fmt.Sprint(out.Sessions), "pats": fmt.Sprint(out.PATs), "passkeys": fmt.Sprint(out.Passkeys)})
	return out, nil
}

func (u *Users) revokeSessions(ctx context.Context, id domain.UserID) (int, error) {
	if u.Sessions == nil {
		return 0, nil
	}
	list, _ := u.Sessions.ListForUser(ctx, id)
	if err := u.Sessions.RevokeAllForUser(ctx, id); err != nil {
		return 0, internalErr("revoke sessions", err)
	}
	return len(list), nil
}

func (u *Users) revokePATs(ctx context.Context, id domain.UserID) (int, error) {
	if u.PATs == nil {
		return 0, nil
	}
	list, err := u.PATs.ListForUser(ctx, id)
	if err != nil {
		return 0, internalErr("list tokens", err)
	}
	n := 0
	for _, p := range list {
		if p.RevokedAt != nil {
			continue
		}
		if err := u.PATs.Revoke(ctx, id, p.ID); err != nil {
			return n, internalErr("revoke token", err)
		}
		n++
	}
	return n, nil
}

func (u *Users) removePasskeys(ctx context.Context, id domain.UserID) (int, error) {
	if u.Passkeys == nil {
		return 0, nil
	}
	list, err := u.Passkeys.ListByUser(ctx, id)
	if err != nil {
		return 0, internalErr("list passkeys", err)
	}
	for i, k := range list {
		if err := u.Passkeys.Delete(ctx, id, k.ID); err != nil {
			return i, internalErr("delete passkey", err)
		}
	}
	_ = u.Users.SetPasskeySecondFactor(ctx, id, false)
	return len(list), nil
}

// notLastAdmin refuses an action that would leave no active admin.
func (u *Users) notLastAdmin(ctx context.Context) error {
	n, err := u.Users.CountAdmins(ctx)
	if err != nil {
		return internalErr("count admins", err)
	}
	if n <= 1 {
		return port.NewActionError(port.ActionErrorPrecondition, port.ErrLastAdmin.Error())
	}
	return nil
}

// SetQuota sets or clears a user's activity cap; nil clears the override,
// zero means unlimited.
func (u *Users) SetQuota(ctx context.Context, actor audit.Actor, id domain.UserID, maxActivities *int) error {
	if u.Quotas == nil {
		return port.NewActionError(port.ActionErrorUnavailable, "quotas are not available")
	}
	if _, err := u.load(ctx, id); err != nil {
		return err
	}
	if maxActivities != nil && *maxActivities < 0 {
		return invalid(errors.New("max_activities must not be negative"))
	}
	if err := u.Quotas.SetMaxActivities(ctx, id, maxActivities); err != nil {
		return internalErr("set quota", err)
	}
	val := "default"
	if maxActivities != nil {
		val = fmt.Sprint(*maxActivities)
	}
	u.Audit.Info(ctx, actor, domain.AuditQuotaChanged, "user", id.String(), map[string]string{"max_activities": val})
	return nil
}
