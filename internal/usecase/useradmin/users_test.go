package useradmin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

type fakeUsers struct {
	port.UserRepo
	rows     map[domain.UserID]*domain.User
	admins   int
	password map[domain.UserID]string
	must     map[domain.UserID]bool
	deleted  []domain.UserID
}

func (f *fakeUsers) GetUser(_ context.Context, id domain.UserID) (domain.User, error) {
	if u, ok := f.rows[id]; ok {
		return *u, nil
	}
	return domain.User{}, domain.ErrNotFound
}
func (f *fakeUsers) CountAdmins(context.Context) (int, error) { return f.admins, nil }
func (f *fakeUsers) UpdateUserRole(_ context.Context, id domain.UserID, role domain.UserRole) error {
	f.rows[id].Role = role
	return nil
}
func (f *fakeUsers) UpdateUserStatus(_ context.Context, id domain.UserID, st domain.UserStatus, reason string) error {
	f.rows[id].Status, f.rows[id].StatusReason = st, reason
	return nil
}
func (f *fakeUsers) UpdateEmail(_ context.Context, id domain.UserID, email string, verified bool) error {
	if email == "taken@x.test" {
		return port.ErrEmailTaken
	}
	f.rows[id].Email, f.rows[id].EmailVerified = email, verified
	return nil
}
func (f *fakeUsers) UpdateProfile(_ context.Context, id domain.UserID, p port.ProfileUpdate) error {
	u := f.rows[id]
	u.DisplayName, u.Locale, u.Timezone, u.Units = p.DisplayName, p.Locale, p.Timezone, domain.UserUnits(p.Units)
	return nil
}
func (f *fakeUsers) UpdatePassword(_ context.Context, id domain.UserID, enc string) error {
	f.password[id] = enc
	return nil
}
func (f *fakeUsers) SetMustChangePassword(_ context.Context, id domain.UserID, must bool) error {
	f.must[id] = must
	return nil
}
func (f *fakeUsers) HardDeleteUser(_ context.Context, id domain.UserID) error {
	delete(f.rows, id)
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeUsers) SetPasskeySecondFactor(context.Context, domain.UserID, bool) error { return nil }

type fakeSessions struct {
	port.SessionRepo
	active     map[domain.UserID]int
	revokedAll []domain.UserID
}

func (f *fakeSessions) ListForUser(_ context.Context, id domain.UserID) ([]domain.Session, error) {
	return make([]domain.Session, f.active[id]), nil
}
func (f *fakeSessions) RevokeAllForUser(_ context.Context, id domain.UserID) error {
	f.revokedAll = append(f.revokedAll, id)
	f.active[id] = 0
	return nil
}

type fakePATs struct {
	port.PATRepo
	rows    []domain.PersonalAccessToken
	revoked int
}

func (f *fakePATs) ListForUser(context.Context, domain.UserID) ([]domain.PersonalAccessToken, error) {
	return f.rows, nil
}
func (f *fakePATs) Revoke(context.Context, domain.UserID, domain.PATID) error {
	f.revoked++
	return nil
}

type fakePasskeys struct {
	port.PasskeyRepo
	rows    []domain.Passkey
	deleted int
}

func (f *fakePasskeys) ListByUser(context.Context, domain.UserID) ([]domain.Passkey, error) {
	return f.rows, nil
}
func (f *fakePasskeys) Delete(context.Context, domain.UserID, domain.PasskeyID) error {
	f.deleted++
	return nil
}

type recordingAudit struct {
	port.AuditRepo
	actions []string
}

func (r *recordingAudit) Record(_ context.Context, e domain.AuditEntry) error {
	r.actions = append(r.actions, e.Action)
	return nil
}

type hasher struct{}

func (hasher) Hash(pw string) (string, error) { return "h:" + pw, nil }

func kindOf(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

func fixture() (*Users, *fakeUsers, *fakeSessions, *recordingAudit, domain.UserID, domain.UserID) {
	admin, bob := domain.UserID(uuid.New()), domain.UserID(uuid.New())
	users := &fakeUsers{
		rows: map[domain.UserID]*domain.User{
			admin: {ID: admin, Username: "admin", Role: domain.UserRoleAdmin, Status: domain.UserStatusActive},
			bob:   {ID: bob, Username: "bob", Email: "bob@x.test", DisplayName: "Bob", Role: domain.UserRoleUser, Status: domain.UserStatusActive, Locale: "en", Timezone: "UTC", Units: domain.UserUnitsMetric},
		},
		admins: 1, password: map[domain.UserID]string{}, must: map[domain.UserID]bool{},
	}
	sessions := &fakeSessions{active: map[domain.UserID]int{bob: 2}}
	rec := &recordingAudit{}
	u := &Users{Users: users, Sessions: sessions, Hasher: hasher{}, Audit: &audit.Recorder{Repo: rec}}
	return u, users, sessions, rec, admin, bob
}

func TestUsersRoleStatusDelete(t *testing.T) {
	u, users, sessions, rec, admin, bob := fixture()
	actor := audit.Actor{UserID: &admin, Username: "admin"}
	ctx := t.Context()

	// Promote bob, then the only other admin cannot be demoted back below one.
	if _, err := u.ChangeRole(ctx, actor, bob, domain.UserRoleAdmin); err != nil {
		t.Fatal(err)
	}
	users.admins = 2
	if _, err := u.ChangeRole(ctx, actor, bob, domain.UserRoleUser); err != nil {
		t.Fatal(err)
	}
	users.admins = 1
	if _, err := u.ChangeRole(ctx, actor, admin, domain.UserRoleUser); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("last admin: %v", err)
	}
	if _, err := u.ChangeRole(ctx, actor, bob, "boss"); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("bad role: %v", err)
	}

	if _, err := u.SetStatus(ctx, actor, admin, domain.UserStatusSuspended, ""); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("self suspend: %v", err)
	}
	if _, err := u.SetStatus(ctx, actor, bob, domain.UserStatusDeleted, ""); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("status deleted via SetStatus: %v", err)
	}
	got, err := u.SetStatus(ctx, actor, bob, domain.UserStatusSuspended, "spam")
	if err != nil || got.Status != domain.UserStatusSuspended || len(sessions.revokedAll) != 1 {
		t.Fatalf("suspend: %v %+v %v", err, got, sessions.revokedAll)
	}

	if _, err := u.Delete(ctx, actor, bob, "alice", false); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("wrong confirm: %v", err)
	}
	if _, err := u.Delete(ctx, actor, admin, "admin", false); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("self delete: %v", err)
	}
	if _, err := u.Delete(ctx, actor, bob, "BOB", false); err != nil || users.rows[bob].Status != domain.UserStatusDeleted {
		t.Fatalf("soft delete: %v", err)
	}
	users.rows[bob].Status = domain.UserStatusActive
	if _, err := u.Delete(ctx, actor, bob, "bob", true); err != nil || len(users.deleted) != 1 {
		t.Fatalf("hard delete: %v", err)
	}
	if _, err := u.Get(ctx, bob); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("gone: %v", err)
	}
	want := []string{domain.AuditUserRoleChanged, domain.AuditUserRoleChanged, domain.AuditUserStatusChanged, domain.AuditUserDeleted, domain.AuditUserDeleted}
	if len(rec.actions) != len(want) {
		t.Fatalf("audit %v", rec.actions)
	}
	for i := range want {
		if rec.actions[i] != want[i] {
			t.Fatalf("audit %v", rec.actions)
		}
	}
}

func TestUsersUpdateResetRevoke(t *testing.T) {
	u, users, sessions, _, admin, bob := fixture()
	actor := audit.Actor{UserID: &admin}
	ctx := t.Context()
	str := func(s string) *string { return &s }

	if _, err := u.Update(ctx, actor, bob, UserPatch{Email: str("taken@x.test")}); kindOf(t, err) != port.ActionErrorConflict {
		t.Fatalf("email taken: %v", err)
	}
	if _, err := u.Update(ctx, actor, bob, UserPatch{Timezone: str("Mars/Base")}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("bad tz: %v", err)
	}
	got, err := u.Update(ctx, actor, bob, UserPatch{Email: str("Bob2@x.test"), DisplayName: str(" Robert "), Units: str("imperial")})
	if err != nil || got.Email != "bob2@x.test" || got.EmailVerified || got.DisplayName != "Robert" || got.Units != domain.UserUnitsImperial {
		t.Fatalf("update: %v %+v", err, got)
	}

	temp, err := u.ResetPassword(ctx, actor, bob)
	if err != nil || temp == "" || users.password[bob] != "h:"+temp || !users.must[bob] || len(sessions.revokedAll) != 1 {
		t.Fatalf("reset: %v %q", err, temp)
	}

	gone := time.Now()
	pats := &fakePATs{rows: []domain.PersonalAccessToken{{ID: domain.PATID(uuid.New())}, {ID: domain.PATID(uuid.New()), RevokedAt: &gone}}}
	keys := &fakePasskeys{rows: []domain.Passkey{{ID: domain.PasskeyID(uuid.New())}}}
	u.PATs, u.Passkeys = pats, keys
	sessions.active[bob] = 3
	res, err := u.RevokeCredentials(ctx, actor, bob, false, false, false)
	if err != nil || res.Sessions != 3 || res.PATs != 1 || res.Passkeys != 1 || pats.revoked != 1 || keys.deleted != 1 {
		t.Fatalf("revoke all: %v %+v", err, res)
	}
	res, err = u.RevokeCredentials(ctx, actor, bob, true, false, false)
	if err != nil || res.PATs != 0 || res.Passkeys != 0 {
		t.Fatalf("revoke sessions only: %v %+v", err, res)
	}
	if _, err := u.ResetPassword(ctx, actor, domain.UserID(uuid.New())); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("unknown: %v", err)
	}
}
