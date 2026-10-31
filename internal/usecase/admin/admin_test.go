package admin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/auth"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// fakeUsers is an in-memory user store keyed by email.
type fakeUsers struct {
	port.UserLifecycleStore
	port.UserReader
	byEmail   map[string]domain.User
	creds     map[domain.UserID]domain.Credentials
	admins    int
	createErr error
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{byEmail: map[string]domain.User{}, creds: map[domain.UserID]domain.Credentials{}}
}

func (f *fakeUsers) CreateUser(_ context.Context, u domain.User, cred domain.Credentials) error {
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.byEmail[u.Email]; ok {
		return port.ErrEmailTaken
	}
	f.byEmail[u.Email] = u
	f.creds[u.ID] = cred
	if u.Role == domain.UserRoleAdmin {
		f.admins++
	}
	return nil
}

func (f *fakeUsers) GetUserByEmail(_ context.Context, email string) (domain.User, error) {
	if u, ok := f.byEmail[email]; ok {
		return u, nil
	}
	return domain.User{}, domain.ErrNotFound
}

func (f *fakeUsers) CountAdmins(context.Context) (int, error) { return f.admins, nil }

// cheap argon2 params keep the tests fast.
func testHasher() *auth.PasswordHasher {
	return auth.NewPasswordHasher(auth.PasswordParams{Time: 1, Memory: 64, Threads: 1, KeyLen: 16, SaltLen: 8})
}

var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func newCreate(users *fakeUsers) *CreateUser {
	id := uuid.MustParse("01900000-0000-7000-8000-000000000001")
	return NewCreateUser(users, testHasher(), func() time.Time { return fixedNow }, func() uuid.UUID { return id })
}

func TestCreateUser_DefaultsAndNormalisation(t *testing.T) {
	users := newFakeUsers()
	res, err := newCreate(users).Execute(context.Background(), CreateUserInput{
		Username: "  Alice ", Email: " Alice@Example.COM ", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	u := res.User
	if u.Username != "alice" || u.Email != "alice@example.com" {
		t.Errorf("not normalised: %q %q", u.Username, u.Email)
	}
	if u.Role != domain.UserRoleUser || u.Units != domain.UserUnitsMetric || u.Locale != "en" || u.Timezone != "UTC" {
		t.Errorf("defaults not applied: %+v", u)
	}
	if u.Status != domain.UserStatusActive || !u.CreatedAt.Equal(fixedNow) {
		t.Errorf("status/created: %v %v", u.Status, u.CreatedAt)
	}
	if users.creds[u.ID].PasswordHash != "" {
		t.Error("no password given, but a hash was stored")
	}
}

func TestCreateUser_PasswordIsHashed(t *testing.T) {
	users := newFakeUsers()
	h := testHasher()
	uc := NewCreateUser(users, h, nil, nil)
	res, err := uc.Execute(context.Background(), CreateUserInput{
		Username: "bob", Email: "bob@example.com", DisplayName: "Bob", Password: "s3cret-pass", MustChangePassword: true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.User.MustChangePassword {
		t.Error("MustChangePassword dropped")
	}
	cred := users.creds[res.User.ID]
	if cred.UserID != res.User.ID || cred.PasswordHash == "" || cred.PasswordHash == "s3cret-pass" {
		t.Fatalf("credentials not hashed: %+v", cred)
	}
	if ok, err := h.Verify("s3cret-pass", cred.PasswordHash); err != nil || !ok {
		t.Errorf("stored hash does not verify: %v %v", ok, err)
	}
	if res.User.ID == (domain.UserID{}) {
		t.Error("default id generator produced zero id")
	}
}

func TestCreateUser_Rejects(t *testing.T) {
	cases := map[string]struct {
		in   CreateUserInput
		want error
	}{
		"bad username": {CreateUserInput{Username: "ab", Email: "a@example.com", DisplayName: "A"}, domain.ErrUsernameTooShort},
		"bad email":    {CreateUserInput{Username: "abc", Email: "nope", DisplayName: "A"}, domain.ErrEmailInvalid},
		"bad role":     {CreateUserInput{Username: "abc", Email: "a@example.com", DisplayName: "A", Role: "root"}, domain.ErrInvalidUserRole},
		"no display":   {CreateUserInput{Username: "abc", Email: "a@example.com"}, domain.ErrDisplayNameRequired},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			users := newFakeUsers()
			_, err := newCreate(users).Execute(context.Background(), tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(users.byEmail) != 0 {
				t.Error("invalid input was persisted")
			}
		})
	}
}

func TestCreateUser_RepoErrors(t *testing.T) {
	in := CreateUserInput{Username: "abc", Email: "a@example.com", DisplayName: "A"}

	users := newFakeUsers()
	users.createErr = port.ErrUsernameTaken
	if _, err := newCreate(users).Execute(context.Background(), in); !errors.Is(err, port.ErrUsernameTaken) {
		t.Errorf("taken: %v", err)
	}

	boom := errors.New("boom")
	users.createErr = boom
	if _, err := newCreate(users).Execute(context.Background(), in); !errors.Is(err, boom) {
		t.Errorf("wrapped: %v", err)
	}
}

func TestBootstrapAdmin(t *testing.T) {
	ctx := context.Background()
	in := BootstrapAdminInput{Email: "Root@Example.com", Password: "pw-123456"}

	t.Run("admin present", func(t *testing.T) {
		users := newFakeUsers()
		users.admins = 2
		res, err := NewBootstrapAdmin(users, newCreate(users)).Execute(ctx, in)
		if err != nil || res.Action != BootstrapAdminAlreadyPresent || res.AdminCount != 2 {
			t.Fatalf("got %+v, %v", res, err)
		}
	})

	t.Run("missing input", func(t *testing.T) {
		users := newFakeUsers()
		res, err := NewBootstrapAdmin(users, newCreate(users)).Execute(ctx, BootstrapAdminInput{Email: "x@example.com"})
		if err != nil || res.Action != BootstrapAdminMissingInput {
			t.Fatalf("got %+v, %v", res, err)
		}
		if len(users.byEmail) != 0 {
			t.Error("created a user without a password")
		}
	})

	t.Run("first admin created", func(t *testing.T) {
		users := newFakeUsers()
		res, err := NewBootstrapAdmin(users, newCreate(users)).Execute(ctx, in)
		if err != nil || res.Action != BootstrapAdminCreated {
			t.Fatalf("got %+v, %v", res, err)
		}
		u := res.CreatedUser
		if u.Role != domain.UserRoleAdmin || !u.MustChangePassword || !u.EmailVerified {
			t.Errorf("admin flags: %+v", u)
		}
		if u.Username != "root" || u.DisplayName != "root" || u.Email != "root@example.com" {
			t.Errorf("derived names: %q %q %q", u.Username, u.DisplayName, u.Email)
		}
		if users.creds[u.ID].PasswordHash == "" {
			t.Error("password not stored")
		}
	})

	t.Run("existing non-admin refused", func(t *testing.T) {
		users := newFakeUsers()
		users.byEmail["root@example.com"] = domain.User{ID: domain.UserID(uuid.New()), Email: "root@example.com"}
		_, err := NewBootstrapAdmin(users, newCreate(users)).Execute(ctx, in)
		if err == nil {
			t.Fatal("silently promoted an existing user")
		}
		if users.admins != 0 {
			t.Error("admin count changed")
		}
	})
}

func TestDeriveUsername(t *testing.T) {
	cases := map[string]string{
		"jane.doe@example.com": "jane.doe",
		"ab@example.com":       "admin", // too short
		"@example.com":         "admin",
	}
	for in, want := range cases {
		if got := deriveUsername(in); got != want {
			t.Errorf("deriveUsername(%q) = %q, want %q", in, got, want)
		}
	}
}
