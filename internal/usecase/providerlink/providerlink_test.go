package providerlink

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/port"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type published struct{ subject, msgID string }

type fakeBus struct {
	port.JobBus
	sent []published
	fail error
}

func (b *fakeBus) Publish(_ context.Context, subject, msgID string, _ []byte, _ ...port.PublishOpt) error {
	if b.fail != nil {
		return b.fail
	}
	b.sent = append(b.sent, published{subject, msgID})
	return nil
}

type fakeAccounts struct {
	port.ExternalAccountRepo
	byID    map[domain.ExternalAccountID]domain.ExternalAccount
	created []domain.ExternalAccount
	failNew error
}

func (f *fakeAccounts) HeldByOtherUser(_ context.Context, provider, providerAcct string, user domain.UserID) (bool, error) {
	for _, a := range f.byID {
		if a.Provider == provider && a.ProviderAccountID == providerAcct && a.UserID != user {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeAccounts) GetExternalAccount(_ context.Context, id domain.ExternalAccountID) (domain.ExternalAccount, error) {
	a, ok := f.byID[id]
	if !ok {
		return a, domain.ErrNotFound
	}
	return a, nil
}
func (f *fakeAccounts) CreateAccount(_ context.Context, a domain.ExternalAccount) (domain.ExternalAccountID, error) {
	if f.failNew != nil {
		return domain.ExternalAccountID{}, f.failNew
	}
	a.ID = domain.ExternalAccountID(uuid.New())
	f.created = append(f.created, a)
	if f.byID == nil {
		f.byID = map[domain.ExternalAccountID]domain.ExternalAccount{}
	}
	f.byID[a.ID] = a
	return a.ID, nil
}
func (f *fakeAccounts) ListAccountsForUser(context.Context, domain.UserID) ([]domain.ExternalAccount, error) {
	return nil, nil
}

type fakeConfigs struct {
	port.UserProviderConfigRepo
	byID    map[domain.ConnectionID]domain.UserProviderConfig
	deleted []domain.ConnectionID
}

func (f *fakeConfigs) Create(_ context.Context, c domain.UserProviderConfig) (domain.ConnectionID, error) {
	c.ID = domain.ConnectionID(uuid.New())
	if f.byID == nil {
		f.byID = map[domain.ConnectionID]domain.UserProviderConfig{}
	}
	f.byID[c.ID] = c
	return c.ID, nil
}
func (f *fakeConfigs) GetByID(_ context.Context, id domain.ConnectionID) (domain.UserProviderConfig, error) {
	c, ok := f.byID[id]
	if !ok {
		return c, domain.ErrNotFound
	}
	return c, nil
}
func (f *fakeConfigs) Delete(_ context.Context, id domain.ConnectionID) error {
	f.deleted = append(f.deleted, id)
	delete(f.byID, id)
	return nil
}

type fakeTokens struct {
	port.OAuthTokenRepo
	stored  map[domain.ExternalAccountID]port.TokenState
	flagged map[domain.ExternalAccountID]string
}

func (f *fakeTokens) GetToken(_ context.Context, id domain.ExternalAccountID) (port.TokenState, error) {
	st, ok := f.stored[id]
	if !ok {
		return st, port.ErrTokenAccountNotFound
	}
	return st, nil
}
func (f *fakeTokens) StoreToken(_ context.Context, id domain.ExternalAccountID, in port.StoreTokenInput) error {
	if f.stored == nil {
		f.stored = map[domain.ExternalAccountID]port.TokenState{}
	}
	f.stored[id] = in.State
	return nil
}
func (f *fakeTokens) MarkNeedsReauth(_ context.Context, id domain.ExternalAccountID, reason string) error {
	if f.flagged == nil {
		f.flagged = map[domain.ExternalAccountID]string{}
	}
	f.flagged[id] = reason
	return nil
}

// fakePools is one pool per provider, "main", optionally declaring gear.
type fakePools struct{ gear bool }

func (p fakePools) KnownPoolNames(context.Context, string) []string { return []string{"main"} }
func (p fakePools) PoolDeclares(_ context.Context, _, pool string, dt capability.DataType) bool {
	return p.gear && pool == "main" && dt == capability.DataTypeGear
}

type fixedRouter struct{ err error }

func (r fixedRouter) AccountWorker(context.Context, domain.ExternalAccount) (string, error) {
	return "main", r.err
}

type fakeExchanger struct{ err error }

func (e fakeExchanger) ExchangeCode(context.Context, string, domain.UserProviderConfig, string) (port.TokenState, string, error) {
	return port.TokenState{AccessToken: "at"}, "4242", e.err
}

func newAccount(accts *fakeAccounts, owner domain.UserID) domain.ExternalAccount {
	a := domain.ExternalAccount{ID: domain.ExternalAccountID(uuid.New()), UserID: owner, Provider: "strava"}
	if accts.byID == nil {
		accts.byID = map[domain.ExternalAccountID]domain.ExternalAccount{}
	}
	accts.byID[a.ID] = a
	return a
}

func actionKind(err error) port.ActionErrorKind {
	var ae *port.ActionError
	if errors.As(err, &ae) {
		return ae.Kind
	}
	return -1
}

func TestImportGearNeedsADeclaringPool(t *testing.T) {
	ctx := context.Background()
	accts := &fakeAccounts{}
	bus := &fakeBus{}
	owner := domain.UserID(uuid.New())
	acct := newAccount(accts, owner)
	j := &Jobs{Accounts: accts, Bus: bus, Router: fixedRouter{}, Pools: fakePools{}, Log: discard,
		Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }}

	if err := j.SyncGear(ctx, owner, acct.ID); !errors.Is(err, ErrGearNotSupported) {
		t.Fatalf("no gear pool: %v", err)
	}
	if err := j.ScheduledGearSync(ctx, acct); err != nil || len(bus.sent) != 0 {
		t.Fatalf("scheduled sync must skip silently: %v %v", err, bus.sent)
	}
	j.Pools = fakePools{gear: true}
	if err := j.SyncGear(ctx, owner, acct.ID); err != nil {
		t.Fatal(err)
	}
	want := published{"cairn.jobs.import_gear.strava.main", "import_gear:" + acct.ID.String() + ":2026-01-02T03:04"}
	if len(bus.sent) != 1 || bus.sent[0] != want {
		t.Fatalf("published %v", bus.sent)
	}
	if err := j.SyncGear(ctx, domain.UserID(uuid.New()), acct.ID); !errors.Is(err, errAccountGone) {
		t.Fatalf("foreign account: %v", err)
	}
	if err := (&Jobs{}).ImportGear(ctx, acct, "manual"); !errors.Is(err, errNoBus) {
		t.Fatalf("no bus: %v", err)
	}
}

func TestImportHealth(t *testing.T) {
	ctx := context.Background()
	accts := &fakeAccounts{}
	bus := &fakeBus{}
	owner := domain.UserID(uuid.New())
	acct := newAccount(accts, owner)
	j := &Jobs{Accounts: accts, Bus: bus, Router: fixedRouter{}, Log: discard}
	if err := j.ImportHealth(ctx, owner, acct.ID, 7); err != nil || len(bus.sent) != 1 || bus.sent[0].subject != "cairn.jobs.import_metrics.strava.main" {
		t.Fatalf("import health: %v %v", err, bus.sent)
	}
	j.Router = fixedRouter{err: domain.ErrNoWorker}
	if k := actionKind(j.ImportHealth(ctx, owner, acct.ID, 7)); k != port.ActionErrorConflict {
		t.Fatalf("no pool: kind %v", k)
	}
	if k := actionKind((&Jobs{}).ImportHealth(ctx, owner, acct.ID, 7)); k != port.ActionErrorUnavailable {
		t.Fatalf("no bus: kind %v", k)
	}
	for in, want := range map[int]int{0: 30, -1: 30, 1: 1, 365: 365, 366: 30} {
		if got := HealthImportDays(in); got != want {
			t.Errorf("HealthImportDays(%d) = %d, want %d", in, got, want)
		}
	}
}

func newConnections() (*Connections, *fakeConfigs, *fakeAccounts, *fakeTokens) {
	cfgs, toks := &fakeConfigs{}, &fakeTokens{}
	accts := &fakeAccounts{}
	return &Connections{
		Providers: Providers, Configs: cfgs, Accounts: accts, Tokens: toks,
		Pools: fakePools{}, Exchanger: fakeExchanger{}, Log: discard,
	}, cfgs, accts, toks
}

func TestCreateConnection(t *testing.T) {
	ctx := context.Background()
	c, cfgs, accts, _ := newConnections()
	user := domain.UserID(uuid.New())
	for _, in := range []NewConnection{
		{Provider: "nope", ClientID: "x"},
		{Provider: "strava", ClientID: " "},
		{Provider: "garmin", ClientID: "me@example.com"},
	} {
		if k := actionKind(func() error { _, err := c.Create(ctx, user, in); return err }()); k != port.ActionErrorInvalid {
			t.Errorf("%+v: kind %v", in, k)
		}
	}
	// OAuth provider: just the connection.
	if _, err := c.Create(ctx, user, NewConnection{Provider: "strava", ClientID: "123", ClientSecret: "s"}); err != nil || len(accts.created) != 0 {
		t.Fatalf("strava: %v %v", err, accts.created)
	}
	// Credential provider: the account comes with it.
	if _, err := c.Create(ctx, user, NewConnection{Provider: "garmin", ClientID: " me@example.com ", ClientSecret: "pw"}); err != nil {
		t.Fatal(err)
	}
	if len(accts.created) != 1 || accts.created[0].ProviderAccountID != "me@example.com" || accts.created[0].AssignedWorkerName != "main" {
		t.Fatalf("garmin account: %+v", accts.created)
	}
	// A failed link rolls the connection back.
	accts.failNew = errors.New("boom")
	if _, err := c.Create(ctx, user, NewConnection{Provider: "garmin", ClientID: "b@example.com", ClientSecret: "pw"}); err == nil || len(cfgs.deleted) != 1 {
		t.Fatalf("rollback: %v %v", err, cfgs.deleted)
	}
}

func TestConnectRoundTrip(t *testing.T) {
	ctx := context.Background()
	c, cfgs, accts, toks := newConnections()
	user := domain.UserID(uuid.New())
	id, _ := cfgs.Create(ctx, domain.UserProviderConfig{UserID: user, Provider: "strava", ClientID: "123"})
	strava, err := c.OAuthProvider("strava")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.OAuthProvider("garmin"); !errors.Is(err, ErrNotOAuth) {
		t.Fatalf("garmin: %v", err)
	}

	target, err := c.AuthorizeURL(ctx, user, strava, id, "", "st", "https://cairn.test/cb")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(target)
	if u.Host != "www.strava.com" || u.Query().Get("state") != "st" || u.Query().Get("client_id") != "123" {
		t.Fatalf("authorize url %q", target)
	}
	if _, err := c.AuthorizeURL(ctx, domain.UserID(uuid.New()), strava, id, "", "st", ""); !errors.Is(err, ErrConfigureFirst) {
		t.Fatalf("foreign connection: %v", err)
	}
	if _, err := c.AuthorizeURL(ctx, user, strava, id, "bad name!", "st", ""); !errors.Is(err, ErrBadWorker) {
		t.Fatalf("bad worker: %v", err)
	}

	acctID, err := c.Complete(ctx, user, strava, id, "code", "")
	if err != nil {
		t.Fatal(err)
	}
	if a := accts.byID[acctID]; a.ProviderAccountID != "4242" || a.DisplayLabel != "Strava" || len(a.GrantedScopes) != 2 {
		t.Fatalf("account %+v", a)
	}
	if toks.stored[acctID].AccessToken != "at" {
		t.Fatal("token not stored")
	}
	c.Exchanger = fakeExchanger{err: errors.New("denied")}
	if _, err := c.Complete(ctx, user, strava, id, "code", ""); !errors.Is(err, ErrExchangeFailed) {
		t.Fatalf("exchange failure: %v", err)
	}
}

// A provider account belongs to one user: webhooks name only the athlete.
func TestConnectRefusesAccountOfAnotherUser(t *testing.T) {
	ctx := context.Background()
	c, cfgs, accts, _ := newConnections()
	strava, _ := c.OAuthProvider("strava")
	alice, bob := domain.UserID(uuid.New()), domain.UserID(uuid.New())
	aliceConn, _ := cfgs.Create(ctx, domain.UserProviderConfig{UserID: alice, Provider: "strava", ClientID: "app1"})
	first, err := c.Complete(ctx, alice, strava, aliceConn, "code", "")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := c.Complete(ctx, alice, strava, aliceConn, "code", ""); err != nil || again == (domain.ExternalAccountID{}) {
		t.Fatalf("same-user relink: %v", err)
	}

	bobSameApp, _ := cfgs.Create(ctx, domain.UserProviderConfig{UserID: bob, Provider: "strava", ClientID: "app1"})
	if _, err := c.Complete(ctx, bob, strava, bobSameApp, "code", ""); !errors.Is(err, ErrAlreadyLinked) {
		t.Fatalf("other user, same app: %v", err)
	}
	bobOwnApp, _ := cfgs.Create(ctx, domain.UserProviderConfig{UserID: bob, Provider: "strava", ClientID: "app2"})
	if _, err := c.Complete(ctx, bob, strava, bobOwnApp, "code", ""); !errors.Is(err, ErrAlreadyLinked) {
		t.Fatalf("other user, own app: %v", err)
	}
	if _, ok := accts.byID[first]; !ok {
		t.Fatal("first account gone")
	}
}

func TestWorkerTokens(t *testing.T) {
	ctx := context.Background()
	cfgs, accts, toks, bus := &fakeConfigs{}, &fakeAccounts{}, &fakeTokens{}, &fakeBus{}
	connID, _ := cfgs.Create(ctx, domain.UserProviderConfig{Provider: "strava", ClientID: "cid", ClientSecret: "sec"})
	acct := newAccount(accts, domain.UserID(uuid.New()))
	acct.ConnectionID = &connID
	accts.byID[acct.ID] = acct
	_ = toks.StoreToken(ctx, acct.ID, port.StoreTokenInput{State: port.TokenState{AccessToken: "at"}})
	w := &WorkerTokens{Tokens: toks, Accounts: accts, Configs: cfgs, Bus: bus, Log: discard}

	got, err := w.Get(ctx, acct.ID)
	if err != nil || got.State.AccessToken != "at" || got.ClientID != "cid" || got.ClientSecret != "sec" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := w.Get(ctx, domain.ExternalAccountID(uuid.New())); !errors.Is(err, port.ErrTokenAccountNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	if err := w.MarkNeedsReauth(ctx, acct.ID, ""); err != nil || toks.flagged[acct.ID] != "refresh_failed" {
		t.Fatalf("needs reauth: %v %v", err, toks.flagged)
	}
	if len(bus.sent) != 1 || bus.sent[0] != (published{needsReauthSubject, "reauth:" + acct.ID.String()}) {
		t.Fatalf("event %v", bus.sent)
	}
	if email, pw, err := w.Login(ctx, acct.ID); err != nil || email != "cid" || pw != "sec" {
		t.Fatalf("login: %q %q %v", email, pw, err)
	}
	if _, _, err := w.Login(ctx, domain.ExternalAccountID(uuid.New())); !errors.Is(err, ErrAccountGone) {
		t.Fatalf("gone: %v", err)
	}
}
