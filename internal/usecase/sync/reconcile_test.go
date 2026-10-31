package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ---------------------------------------------------------------------------
// Minimal fakes for the two ports the use case depends on
// ---------------------------------------------------------------------------

type fakeAccounts struct {
	byID    map[domain.ExternalAccountID]domain.ExternalAccount
	dueList []domain.ExternalAccount

	knownExtIDs    []string  // returned by ListKnownExternalIDs
	knownErr       error     // forced ListKnownExternalIDs failure
	knownSinceSeen time.Time // records the `since` the use case asked for
}

func (f *fakeAccounts) ListKnownExternalIDs(ctx context.Context, id domain.ExternalAccountID, since time.Time, limit int) ([]string, error) {
	f.knownSinceSeen = since
	if f.knownErr != nil {
		return nil, f.knownErr
	}
	return f.knownExtIDs, nil
}

func (f *fakeAccounts) GetExternalAccount(ctx context.Context, id domain.ExternalAccountID) (domain.ExternalAccount, error) {
	a, ok := f.byID[id]
	if !ok {
		return domain.ExternalAccount{}, fmt.Errorf("not found")
	}
	return a, nil
}

func (f *fakeAccounts) ListAccountsForUser(ctx context.Context, userID domain.UserID) ([]domain.ExternalAccount, error) {
	return nil, nil
}

func (f *fakeAccounts) CreateAccount(ctx context.Context, a domain.ExternalAccount) (domain.ExternalAccountID, error) {
	return a.ID, nil
}

func (f *fakeAccounts) UpdateAccountConfig(ctx context.Context, id domain.ExternalAccountID, patch map[string]any) error {
	return nil
}

func (f *fakeAccounts) FindByProviderAndExternalID(ctx context.Context, provider, externalID, _, pool string) (domain.ExternalAccount, error) {
	for _, a := range f.byID {
		if a.Provider == provider && a.ProviderAccountID == externalID {
			return a, nil
		}
	}
	return domain.ExternalAccount{}, fmt.Errorf("not found")
}

func (f *fakeAccounts) RegisterWebhookSubscription(ctx context.Context, provider, subscriptionID string, accountID domain.ExternalAccountID) error {
	return nil
}

func (f *fakeAccounts) FindBySubscription(ctx context.Context, provider, subscriptionID, _, pool string) (domain.ExternalAccount, error) {
	return domain.ExternalAccount{}, fmt.Errorf("not found")
}

func (f *fakeAccounts) HeldByOtherUser(context.Context, string, string, domain.UserID) (bool, error) {
	return false, nil
}

func (f *fakeAccounts) SetAssignedWorker(ctx context.Context, id domain.ExternalAccountID, name string) error {
	return nil
}

// staticRouter routes every account to one pool.
type staticRouter string

func (r staticRouter) AccountWorker(context.Context, domain.ExternalAccount) (string, error) {
	return string(r), nil
}

func (f *fakeAccounts) SetAutoImport(ctx context.Context, id domain.ExternalAccountID, enabled bool) error {
	return nil
}

func (f *fakeAccounts) ListAccountsDueForReconcile(ctx context.Context, now time.Time, batch int) ([]domain.ExternalAccount, error) {
	if batch >= len(f.dueList) {
		return f.dueList, nil
	}
	return f.dueList[:batch], nil
}

func (f *fakeAccounts) UpdateSyncWatermark(ctx context.Context, id domain.ExternalAccountID, watermark time.Time, succeeded bool, at time.Time) error {
	return nil
}

func (f *fakeAccounts) DeleteAccount(_ context.Context, id domain.ExternalAccountID) error {
	if _, ok := f.byID[id]; !ok {
		return domain.ErrNotFound
	}
	delete(f.byID, id)
	return nil
}

func (f *fakeAccounts) UpdateStatus(ctx context.Context, id domain.ExternalAccountID, s domain.ExternalAccountStatus, reason string) error {
	return nil
}

func (f *fakeAccounts) UpdateRateLimit(ctx context.Context, id domain.ExternalAccountID, s *domain.RateLimitSnapshot) error {
	return nil
}

type publishedJob struct {
	Subject string
	MsgID   string
	Body    []byte
}

type fakeBus struct {
	published     []publishedJob
	publishErr    error
	failOnAccount domain.ExternalAccountID // publish fails when msgID contains this account's UUID
}

func (f *fakeBus) Publish(ctx context.Context, subject, msgID string, body []byte, opts ...port.PublishOpt) error {
	if f.publishErr != nil {
		return f.publishErr
	}
	if f.failOnAccount != (domain.ExternalAccountID{}) &&
		strings.Contains(msgID, f.failOnAccount.String()) {
		return errors.New("simulated publish failure")
	}
	f.published = append(f.published, publishedJob{Subject: subject, MsgID: msgID, Body: body})
	return nil
}

// Stub-out unused JobBus methods.
func (f *fakeBus) Subscribe(context.Context, port.ConsumerConfig, port.MessageHandler) (port.Subscription, error) {
	return nil, nil
}
func (f *fakeBus) Pull(context.Context, port.ConsumerConfig) (port.PullSubscription, error) {
	return nil, nil
}
func (f *fakeBus) Request(context.Context, string, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}
func (f *fakeBus) RespondTo(context.Context, string, port.RequestHandler) (port.Subscription, error) {
	return nil, nil
}
func (f *fakeBus) KV(string) (port.KV, error)                   { return nil, nil }
func (f *fakeBus) ObjectStore(string) (port.ObjectStore, error) { return nil, nil }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustParseUUID(s string) uuid.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

var (
	fixedNow      = time.Date(2026, 5, 17, 12, 34, 56, 0, time.UTC)
	stableUUID    = mustParseUUID("01234567-89ab-7def-8000-000000000001")
	stableAccount = mustParseUUID("01234567-89ab-7def-8000-000000000aaa")
	stableUser    = mustParseUUID("01234567-89ab-7def-8000-000000000bbb")
)

func newUC(t *testing.T, accounts *fakeAccounts, bus *fakeBus) *ReconcileExternalAccount {
	t.Helper()
	return NewReconcileExternalAccount(
		accounts,
		bus,
		staticRouter("w1"),
		nil, // no budget gate in tests
		nil, // no gear sync in tests
		func() time.Time { return fixedNow },
		func() uuid.UUID { return stableUUID },
	)
}

func makeActiveAccount(provider string) domain.ExternalAccount {
	return domain.ExternalAccount{
		ID:                domain.ExternalAccountID(stableAccount),
		UserID:            domain.UserID(stableUser),
		Provider:          provider,
		Status:            domain.ExternalAccountStatusActive,
		AutoImportEnabled: true, // DB default; mirror it in the fixture
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestExecute_InvalidInput_NeitherIDNorAll(t *testing.T) {
	uc := newUC(t, &fakeAccounts{}, &fakeBus{})
	_, err := uc.Execute(context.Background(), ReconcileInput{})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestExecute_SingleAccount_PublishesOne(t *testing.T) {
	id := domain.ExternalAccountID(stableAccount)
	accounts := &fakeAccounts{byID: map[domain.ExternalAccountID]domain.ExternalAccount{
		id: makeActiveAccount("strava"),
	}}
	bus := &fakeBus{}
	uc := newUC(t, accounts, bus)

	res, err := uc.Execute(context.Background(), ReconcileInput{AccountID: &id})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.AccountsScheduled != 1 {
		t.Errorf("scheduled = %d, want 1", res.AccountsScheduled)
	}
	if len(bus.published) != 1 {
		t.Fatalf("published = %d, want 1", len(bus.published))
	}
	p := bus.published[0]
	if p.Subject != "cairn.jobs.reconcile.strava.w1" {
		t.Errorf("subject = %q, want cairn.jobs.reconcile.strava.w1", p.Subject)
	}
	// msgID is bucketed to the minute and contains the account UUID.
	if !strings.Contains(p.MsgID, stableAccount.String()) {
		t.Errorf("msgID = %q, missing account UUID", p.MsgID)
	}
	if !strings.Contains(p.MsgID, "2026-05-17T12:34") {
		t.Errorf("msgID = %q, missing minute-bucket timestamp", p.MsgID)
	}
}

func TestExecute_KnownExtIDsInJobBody(t *testing.T) {
	id := domain.ExternalAccountID(stableAccount)
	watermark := fixedNow.Add(-2 * time.Hour)
	acct := makeActiveAccount("garmin")
	acct.SyncWatermark = &watermark
	accounts := &fakeAccounts{
		byID:        map[domain.ExternalAccountID]domain.ExternalAccount{id: acct},
		knownExtIDs: []string{"garmin-111", "garmin-222"},
	}
	bus := &fakeBus{}
	uc := newUC(t, accounts, bus)

	if _, err := uc.Execute(context.Background(), ReconcileInput{AccountID: &id}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(bus.published) != 1 {
		t.Fatalf("published = %d, want 1", len(bus.published))
	}

	var body struct {
		Watermark   *time.Time `json:"watermark"`
		KnownExtIDs []string   `json:"known_ext_ids"`
	}
	if err := json.Unmarshal(bus.published[0].Body, &body); err != nil {
		t.Fatalf("unmarshal job body: %v", err)
	}
	if len(body.KnownExtIDs) != 2 || body.KnownExtIDs[0] != "garmin-111" {
		t.Errorf("known_ext_ids = %v, want [garmin-111 garmin-222]", body.KnownExtIDs)
	}

	// Watermark newer than the 30d floor governs: since = watermark - 1h,
	// mirroring the workers' drift margin so the newest activity (whose
	// start time IS the watermark) is always in the known set.
	wantSince := watermark.Add(-time.Hour)
	if !accounts.knownSinceSeen.Equal(wantSince) {
		t.Errorf("since = %v, want %v", accounts.knownSinceSeen, wantSince)
	}
}

func TestExecute_KnownExtIDs_LookbackFloorWithoutWatermark(t *testing.T) {
	id := domain.ExternalAccountID(stableAccount)
	accounts := &fakeAccounts{byID: map[domain.ExternalAccountID]domain.ExternalAccount{
		id: makeActiveAccount("garmin"),
	}}
	bus := &fakeBus{}
	uc := newUC(t, accounts, bus)

	if _, err := uc.Execute(context.Background(), ReconcileInput{AccountID: &id}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	wantSince := fixedNow.Add(-30 * 24 * time.Hour)
	if !accounts.knownSinceSeen.Equal(wantSince) {
		t.Errorf("since = %v, want 30d floor %v", accounts.knownSinceSeen, wantSince)
	}
}

func TestExecute_KnownExtIDsLookupFailure_StillPublishes(t *testing.T) {
	id := domain.ExternalAccountID(stableAccount)
	accounts := &fakeAccounts{
		byID:     map[domain.ExternalAccountID]domain.ExternalAccount{id: makeActiveAccount("garmin")},
		knownErr: errors.New("db down"),
	}
	bus := &fakeBus{}
	uc := newUC(t, accounts, bus)

	res, err := uc.Execute(context.Background(), ReconcileInput{AccountID: &id})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.AccountsScheduled != 1 || len(bus.published) != 1 {
		t.Fatalf("scheduled=%d published=%d, want 1/1 (degrade, don't block)",
			res.AccountsScheduled, len(bus.published))
	}
	var body struct {
		KnownExtIDs []string `json:"known_ext_ids"`
	}
	if err := json.Unmarshal(bus.published[0].Body, &body); err != nil {
		t.Fatalf("unmarshal job body: %v", err)
	}
	if len(body.KnownExtIDs) != 0 {
		t.Errorf("known_ext_ids = %v, want empty on lookup failure", body.KnownExtIDs)
	}
}

func TestExecute_AuthInvalid_SkipsWithoutForce(t *testing.T) {
	id := domain.ExternalAccountID(stableAccount)
	acct := makeActiveAccount("strava")
	acct.Status = domain.ExternalAccountStatusAuthInvalid
	accounts := &fakeAccounts{byID: map[domain.ExternalAccountID]domain.ExternalAccount{id: acct}}
	bus := &fakeBus{}
	uc := newUC(t, accounts, bus)

	res, err := uc.Execute(context.Background(), ReconcileInput{AccountID: &id})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.AccountsScheduled != 0 || res.AccountsSkipped != 1 {
		t.Errorf("got scheduled=%d skipped=%d, want 0/1",
			res.AccountsScheduled, res.AccountsSkipped)
	}
	if len(bus.published) != 0 {
		t.Errorf("published = %d, want 0 (auth_invalid)", len(bus.published))
	}
}

func TestExecute_AuthInvalid_PublishesWithForce(t *testing.T) {
	id := domain.ExternalAccountID(stableAccount)
	acct := makeActiveAccount("strava")
	acct.Status = domain.ExternalAccountStatusAuthInvalid
	accounts := &fakeAccounts{byID: map[domain.ExternalAccountID]domain.ExternalAccount{id: acct}}
	bus := &fakeBus{}
	uc := newUC(t, accounts, bus)

	res, err := uc.Execute(context.Background(), ReconcileInput{AccountID: &id, Force: true})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.AccountsScheduled != 1 {
		t.Errorf("scheduled = %d, want 1 (force)", res.AccountsScheduled)
	}
}

func TestExecute_AllMode_UsesBatchSize(t *testing.T) {
	due := []domain.ExternalAccount{
		{ID: domain.ExternalAccountID(mustParseUUID("01234567-89ab-7def-8000-00000000a001")), Provider: "strava", Status: domain.ExternalAccountStatusActive},
		{ID: domain.ExternalAccountID(mustParseUUID("01234567-89ab-7def-8000-00000000a002")), Provider: "strava", Status: domain.ExternalAccountStatusActive},
		{ID: domain.ExternalAccountID(mustParseUUID("01234567-89ab-7def-8000-00000000a003")), Provider: "garmin", Status: domain.ExternalAccountStatusActive},
	}
	accounts := &fakeAccounts{dueList: due}
	bus := &fakeBus{}
	uc := newUC(t, accounts, bus)

	res, err := uc.Execute(context.Background(), ReconcileInput{All: true, BatchSize: 2})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.AccountsScheduled != 2 {
		t.Errorf("scheduled = %d, want 2 (batch=2)", res.AccountsScheduled)
	}
	if len(bus.published) != 2 {
		t.Errorf("published = %d, want 2", len(bus.published))
	}
	// Both should hit cairn.jobs.reconcile.strava (first two in `due`).
	for _, p := range bus.published {
		if p.Subject != "cairn.jobs.reconcile.strava.w1" {
			t.Errorf("subject = %q, want cairn.jobs.reconcile.strava.w1", p.Subject)
		}
	}
}

func TestExecute_AllMode_PerAccountErrorIsolation(t *testing.T) {
	failID := domain.ExternalAccountID(mustParseUUID("01234567-89ab-7def-8000-00000000beef"))
	due := []domain.ExternalAccount{
		{ID: domain.ExternalAccountID(mustParseUUID("01234567-89ab-7def-8000-00000000a001")), Provider: "strava", Status: domain.ExternalAccountStatusActive},
		{ID: failID, Provider: "strava", Status: domain.ExternalAccountStatusActive},
		{ID: domain.ExternalAccountID(mustParseUUID("01234567-89ab-7def-8000-00000000a003")), Provider: "strava", Status: domain.ExternalAccountStatusActive},
	}
	accounts := &fakeAccounts{dueList: due}
	bus := &fakeBus{failOnAccount: failID}
	uc := newUC(t, accounts, bus)

	res, err := uc.Execute(context.Background(), ReconcileInput{All: true})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.AccountsScheduled != 2 {
		t.Errorf("scheduled = %d, want 2 (one failed, batch continues)",
			res.AccountsScheduled)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %d, want 1", len(res.Errors))
	}
	if res.Errors[0].AccountID != failID {
		t.Errorf("failed AccountID = %s, want %s", res.Errors[0].AccountID, failID)
	}
}

func (f *fakeBus) RespondToSubject(context.Context, string, port.SubjectRequestHandler) (port.Subscription, error) {
	return nil, nil
}

type errRouter struct{ err error }

func (r errRouter) AccountWorker(context.Context, domain.ExternalAccount) (string, error) {
	return "", r.err
}

func TestExecute_NoPoolYet_SkipsQuietly(t *testing.T) {
	id := domain.ExternalAccountID(stableAccount)
	accounts := &fakeAccounts{byID: map[domain.ExternalAccountID]domain.ExternalAccount{id: makeActiveAccount("strava")}}
	bus := &fakeBus{}
	uc := NewReconcileExternalAccount(accounts, bus, errRouter{domain.ErrNoWorker}, nil, nil,
		func() time.Time { return fixedNow }, func() uuid.UUID { return stableUUID })
	res, err := uc.Execute(context.Background(), ReconcileInput{AccountID: &id})
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountsSkipped != 1 || len(res.Errors) != 0 || res.AccountsScheduled != 0 {
		t.Fatalf("res = %+v", res)
	}
	// Any other routing failure is an error for that account.
	uc = NewReconcileExternalAccount(accounts, bus, errRouter{domain.ErrWorkerAmbiguous}, nil, nil,
		func() time.Time { return fixedNow }, func() uuid.UUID { return stableUUID })
	res, _ = uc.Execute(context.Background(), ReconcileInput{AccountID: &id})
	if len(res.Errors) != 1 {
		t.Fatalf("res = %+v", res)
	}
}
