package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ---- fakes ----

type fakePATs struct {
	port.PATRepo
	created []domain.PersonalAccessToken
	hashes  [][]byte
	revoked []domain.PATID
}

func (f *fakePATs) Create(_ context.Context, p domain.PersonalAccessToken, hash []byte) (domain.PATID, error) {
	id := domain.PATID(uuid.New())
	p.ID = id
	f.created = append(f.created, p)
	f.hashes = append(f.hashes, hash)
	return id, nil
}
func (f *fakePATs) ListForUser(context.Context, domain.UserID) ([]domain.PersonalAccessToken, error) {
	return f.created, nil
}
func (f *fakePATs) Revoke(_ context.Context, _ domain.UserID, id domain.PATID) error {
	f.revoked = append(f.revoked, id)
	return nil
}

type fakeSessions struct {
	port.SessionRepo
	active     []domain.Session
	revoked    []domain.SessionID
	revokedAll []domain.UserID
}

func (f *fakeSessions) ListForUser(_ context.Context, uid domain.UserID) ([]domain.Session, error) {
	var out []domain.Session
	for _, s := range f.active {
		if s.UserID == uid {
			out = append(out, s)
		}
	}
	return out, nil
}
func (f *fakeSessions) Revoke(_ context.Context, id domain.SessionID) error {
	f.revoked = append(f.revoked, id)
	return nil
}
func (f *fakeSessions) RevokeAllForUser(_ context.Context, uid domain.UserID) error {
	f.revokedAll = append(f.revokedAll, uid)
	return nil
}

type fakeUsers struct {
	port.UserReader
	port.UserCredentialStore
	port.UserProfileWriter
	user     domain.User
	hash     string
	password string
	profile  *port.ProfileUpdate
}

func (f *fakeUsers) GetUser(_ context.Context, id domain.UserID) (domain.User, error) {
	if id != f.user.ID {
		return domain.User{}, domain.ErrNotFound
	}
	return f.user, nil
}
func (f *fakeUsers) GetUserByEmail(_ context.Context, email string) (domain.User, error) {
	if email != f.user.Email {
		return domain.User{}, domain.ErrNotFound
	}
	return f.user, nil
}
func (f *fakeUsers) GetCredentialsByLogin(context.Context, string) (domain.User, domain.Credentials, error) {
	return f.user, domain.Credentials{UserID: f.user.ID, PasswordHash: f.hash}, nil
}
func (f *fakeUsers) UpdatePassword(_ context.Context, _ domain.UserID, encoded string) error {
	f.password = encoded
	return nil
}
func (f *fakeUsers) UpdateProfile(_ context.Context, _ domain.UserID, p port.ProfileUpdate) error {
	f.profile = &p
	f.user.DisplayName, f.user.AvatarURL = p.DisplayName, p.AvatarURL
	f.user.Locale, f.user.Timezone, f.user.Units = p.Locale, p.Timezone, domain.UserUnits(p.Units)
	return nil
}

// plainHasher "hashes" by prefixing, enough to test the flow.
type plainHasher struct{}

func (plainHasher) Hash(pw string) (string, error) { return "h:" + pw, nil }
func (plainHasher) Verify(pw, enc string) (bool, error) {
	return enc == "h:"+pw, nil
}

type fakeTokens struct {
	port.AuthTokenRepo
	resetFor domain.UserID
	hash     []byte
	consume  map[string]domain.UserID
}

func (f *fakeTokens) CreatePasswordResetToken(_ context.Context, uid domain.UserID, hash []byte, _ time.Time) error {
	f.resetFor, f.hash = uid, hash
	return nil
}
func (f *fakeTokens) ConsumePasswordResetToken(_ context.Context, hash []byte, _ time.Time) (domain.UserID, error) {
	if uid, ok := f.consume[string(hash)]; ok {
		return uid, nil
	}
	return domain.UserID{}, port.ErrAuthTokenInvalid
}

type fakeMail struct{ sent []port.EmailMessage }

func (f *fakeMail) Send(_ context.Context, m port.EmailMessage) error {
	f.sent = append(f.sent, m)
	return nil
}

type fakeIdentities struct {
	port.LinkedIdentityRepo
	links   []domain.LinkedIdentity
	deleted []domain.LinkedIdentityID
}

func (f *fakeIdentities) ListForUser(context.Context, domain.UserID) ([]domain.LinkedIdentity, error) {
	return f.links, nil
}
func (f *fakeIdentities) Delete(_ context.Context, _ domain.UserID, id domain.LinkedIdentityID) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type fakePasskeys struct {
	port.PasskeyRepo
	keys []domain.Passkey
}

func (f *fakePasskeys) ListByUser(context.Context, domain.UserID) ([]domain.Passkey, error) {
	return f.keys, nil
}

func kindOf(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

// ---- tokens ----

func TestTokensCreate(t *testing.T) {
	repo := &fakePATs{}
	tok := &Tokens{PATs: repo}
	uid := domain.UserID(uuid.New())

	if _, _, err := tok.Create(t.Context(), uid, "  ", nil, nil); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("blank name: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if _, _, err := tok.Create(t.Context(), uid, "cli", nil, &past); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("past expiry: %v", err)
	}
	pat, secret, err := tok.Create(t.Context(), uid, " cli ", []string{"activities:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, domain.PATTokenPrefix) || len(secret) != len(domain.PATTokenPrefix)+32 {
		t.Fatalf("secret %q", secret)
	}
	if pat.Name != "cli" || pat.TokenPrefix != secret[:16]+"…" || pat.Scopes[0] != "activities:read" {
		t.Fatalf("pat %+v", pat)
	}
	if len(repo.hashes[0]) != 32 || strings.Contains(string(repo.hashes[0]), secret) {
		t.Fatal("hash must be a sha256, never the plaintext")
	}
	if err := tok.Revoke(t.Context(), uid, pat.ID); err != nil || repo.revoked[0] != pat.ID {
		t.Fatalf("revoke: %v %v", err, repo.revoked)
	}
}

// ---- sessions ----

func TestSessionsRevokeOwnOnly(t *testing.T) {
	me, other := domain.UserID(uuid.New()), domain.UserID(uuid.New())
	mine, theirs := domain.SessionID(uuid.New()), domain.SessionID(uuid.New())
	repo := &fakeSessions{active: []domain.Session{{ID: mine, UserID: me}, {ID: theirs, UserID: other}}}
	s := &Sessions{Repo: repo}

	if err := s.Revoke(t.Context(), me, theirs); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("foreign session: %v", err)
	}
	if err := s.Revoke(t.Context(), me, mine); err != nil || repo.revoked[0] != mine {
		t.Fatalf("own session: %v %v", err, repo.revoked)
	}
	list, err := s.List(t.Context(), me)
	if err != nil || len(list) != 1 || list[0].ID != mine {
		t.Fatalf("list: %v %v", err, list)
	}
}

// ---- passwords ----

func newPasswords(users *fakeUsers, sessions *fakeSessions, tokens *fakeTokens, mail *fakeMail) *Passwords {
	p := &Passwords{Users: users, Hasher: plainHasher{}, Tokens: tokens, Sessions: sessions,
		ResetLink: func(code string) string { return "https://cairn.test/reset-password?code=" + code }}
	if mail != nil {
		p.Mail = mail
	}
	return p
}

func TestPasswordsChange(t *testing.T) {
	uid := domain.UserID(uuid.New())
	users := &fakeUsers{user: domain.User{ID: uid, Username: "alice"}, hash: "h:old-password"}
	keep, other := domain.SessionID(uuid.New()), domain.SessionID(uuid.New())
	sessions := &fakeSessions{active: []domain.Session{{ID: keep, UserID: uid}, {ID: other, UserID: uid}}}
	p := newPasswords(users, sessions, &fakeTokens{}, nil)

	if err := p.Change(t.Context(), uid, keep, "old-password", "short"); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("weak: %v", err)
	}
	if err := p.Change(t.Context(), uid, keep, "wrong", "new-password-1"); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("wrong current: %v", err)
	}
	if users.password != "" {
		t.Fatal("password must not change on a failed verify")
	}
	if err := p.Change(t.Context(), uid, keep, "old-password", "new-password-1"); err != nil {
		t.Fatal(err)
	}
	if users.password != "h:new-password-1" {
		t.Fatalf("stored %q", users.password)
	}
	if len(sessions.revoked) != 1 || sessions.revoked[0] != other {
		t.Fatalf("other sessions revoked: %v", sessions.revoked)
	}

	users.hash = ""
	if err := p.Change(t.Context(), uid, keep, "", "new-password-2"); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("no password: %v", err)
	}
}

func TestPasswordsResetRoundTrip(t *testing.T) {
	uid := domain.UserID(uuid.New())
	users := &fakeUsers{user: domain.User{ID: uid, Username: "alice", Email: "alice@x.test"}}
	sessions := &fakeSessions{}
	tokens := &fakeTokens{consume: map[string]domain.UserID{}}
	mail := &fakeMail{}
	p := newPasswords(users, sessions, tokens, mail)

	// Unknown address: nothing sent, nothing reported.
	if attempted, err := p.RequestReset(t.Context(), "nobody@x.test"); attempted || err != nil {
		t.Fatalf("unknown: %v %v", attempted, err)
	}
	attempted, err := p.RequestReset(t.Context(), " Alice@X.test ")
	if err != nil || !attempted || len(mail.sent) != 1 || tokens.resetFor != uid {
		t.Fatalf("known: %v %v sent=%d for=%s", attempted, err, len(mail.sent), tokens.resetFor)
	}
	body := mail.sent[0].TextBody
	i := strings.Index(body, "code=")
	code := strings.Fields(body[i+len("code="):])[0]
	tokens.consume[string(tokens.hash)] = uid

	if _, err := p.ConfirmReset(t.Context(), "bogus", "new-password-1"); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("bogus code: %v", err)
	}
	got, err := p.ConfirmReset(t.Context(), code, "new-password-1")
	if err != nil || got != uid {
		t.Fatalf("confirm: %v %s", err, got)
	}
	if users.password != "h:new-password-1" || len(sessions.revokedAll) != 1 {
		t.Fatalf("password %q revokedAll %v", users.password, sessions.revokedAll)
	}

	// Mail disabled: generic outcome, no token minted.
	p.Mail = nil
	tokens.resetFor = domain.UserID{}
	if attempted, err := p.RequestReset(t.Context(), "alice@x.test"); attempted || err != nil || tokens.resetFor != (domain.UserID{}) {
		t.Fatalf("mail off: %v %v", attempted, err)
	}
}

// ---- identities ----

func TestIdentitiesUnlinkKeepsLastCredential(t *testing.T) {
	uid := domain.UserID(uuid.New())
	only := domain.LinkedIdentityID(uuid.New())
	repo := &fakeIdentities{links: []domain.LinkedIdentity{{ID: only, UserID: uid, Provider: "google"}}}
	users := &fakeUsers{user: domain.User{ID: uid, Username: "alice"}}
	keys := &fakePasskeys{}
	ids := &Identities{Repo: repo, Users: users, Passkeys: keys,
		Providers: []domain.OIDCProvider{{ID: "google", Name: "Google"}}}

	if err := ids.Unlink(t.Context(), uid, domain.LinkedIdentityID(uuid.New())); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("unknown: %v", err)
	}
	if err := ids.Unlink(t.Context(), uid, only); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("last credential: %v", err)
	}
	keys.keys = []domain.Passkey{{UserID: uid}}
	if err := ids.Unlink(t.Context(), uid, only); err != nil || len(repo.deleted) != 1 {
		t.Fatalf("with passkey: %v %v", err, repo.deleted)
	}
	keys.keys = nil
	users.hash = "h:x"
	if err := ids.Unlink(t.Context(), uid, only); err != nil {
		t.Fatalf("with password: %v", err)
	}
	if ids.ProviderName("google") != "Google" || ids.ProviderName("other") != "other" {
		t.Fatal("provider names")
	}
}

// ---- profile ----

func TestProfilesUpdate(t *testing.T) {
	uid := domain.UserID(uuid.New())
	users := &fakeUsers{user: domain.User{ID: uid, DisplayName: "Alice", Locale: "en", Timezone: "UTC", Units: domain.UserUnitsMetric}}
	p := &Profiles{Users: users}
	str := func(s string) *string { return &s }

	cases := map[string]ProfilePatch{
		"blank name":   {DisplayName: str("  ")},
		"bad avatar":   {AvatarURL: str("ftp://x")},
		"bad timezone": {Timezone: str("Mars/Olympus")},
		"bad locale":   {Locale: str("not a tag!")},
		"bad units":    {Units: str("furlongs")},
	}
	for name, patch := range cases {
		if _, err := p.Update(t.Context(), uid, patch); kindOf(t, err) != port.ActionErrorInvalid {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if users.profile != nil {
		t.Fatal("invalid patches must not write")
	}
	u, err := p.Update(t.Context(), uid, ProfilePatch{DisplayName: str(" Alice B. "), Timezone: str("Europe/Berlin"), Units: str("imperial")})
	if err != nil {
		t.Fatal(err)
	}
	if u.DisplayName != "Alice B." || u.Timezone != "Europe/Berlin" || u.Units != domain.UserUnitsImperial || u.Locale != "en" {
		t.Fatalf("updated %+v", u)
	}
	if _, err := p.Update(t.Context(), domain.UserID(uuid.New()), ProfilePatch{}); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("unknown user: %v", err)
	}
}
