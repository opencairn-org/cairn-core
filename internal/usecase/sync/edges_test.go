package sync

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type budgetKV struct {
	port.KV
	vals map[string]string
}

func (k budgetKV) Get(_ context.Context, key string) (port.KVEntry, error) {
	v, ok := k.vals[key]
	if !ok {
		return port.KVEntry{}, port.ErrKVKeyNotFound
	}
	return port.KVEntry{Key: key, Value: []byte(v)}, nil
}

type budgetBus struct{ kv budgetKV }

func (b budgetBus) KV(string) (port.KV, error) { return b.kv, nil }

func TestBudgets_KeyFallbackAndElapsedWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	bus := budgetBus{budgetKV{vals: map[string]string{
		"strava.app1.short": `{"capacity":100,"used":100,"window_reset_at":"2026-01-01T12:10:00Z"}`,
		"strava.daily":      `{"capacity":1000,"used":900,"window_reset_at":"2026-01-01T11:00:00Z"}`,
	}}}
	b := &Budgets{Bus: bus, Now: func() time.Time { return now }}

	got, exhausted := b.Of(context.Background(), "strava", "app1", "main")
	if !exhausted || got.Short == nil || got.Short.Used != 100 {
		t.Fatalf("short = %+v, exhausted %v", got.Short, exhausted)
	}
	if got.Daily == nil || got.Daily.Used != 0 || got.Daily.Limit != 1000 {
		t.Errorf("elapsed daily window = %+v, want used 0", got.Daily)
	}
	if got, ex := b.Of(context.Background(), "strava", "other", ""); ex || got.Short != nil {
		t.Errorf("other app = %+v, %v", got, ex)
	}
}

// lookupAccounts mimics the repo's routing: candidates matched in memory,
// several of them domain.ErrAmbiguous.
type lookupAccounts struct {
	port.ExternalAccountRepo
	accts      []domain.ExternalAccount
	registered map[domain.ExternalAccountID]string
	pool       string
}

func (f *lookupAccounts) pick(pool string, match func(domain.ExternalAccount) bool) (domain.ExternalAccount, error) {
	f.pool = pool
	var found []domain.ExternalAccount
	for _, a := range f.accts {
		if match(a) && (pool == "" || a.AssignedWorkerName == "" || a.AssignedWorkerName == pool) {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 0:
		return domain.ExternalAccount{}, domain.ErrNotFound
	case 1:
		return found[0], nil
	}
	return domain.ExternalAccount{}, domain.ErrAmbiguous
}

func (f *lookupAccounts) FindBySubscription(_ context.Context, _, sub, owner, pool string) (domain.ExternalAccount, error) {
	return f.pick(pool, func(a domain.ExternalAccount) bool {
		return a.WebhookSubscriptionID == sub && (owner == "" || a.ProviderAccountID == owner)
	})
}

func (f *lookupAccounts) FindByProviderAndExternalID(_ context.Context, _, ext, sub, pool string) (domain.ExternalAccount, error) {
	a, err := f.pick(pool, func(a domain.ExternalAccount) bool {
		return a.ProviderAccountID == ext && (sub == "" || a.WebhookSubscriptionID == "" || a.WebhookSubscriptionID == sub)
	})
	if errors.Is(err, domain.ErrNotFound) && sub != "" {
		return f.pick(pool, func(a domain.ExternalAccount) bool { return a.ProviderAccountID == ext })
	}
	return a, err
}

func (f *lookupAccounts) RegisterWebhookSubscription(_ context.Context, _, sub string, id domain.ExternalAccountID) error {
	if f.registered == nil {
		f.registered = map[domain.ExternalAccountID]string{}
	}
	f.registered[id] = sub
	for i := range f.accts {
		if f.accts[i].ID == id {
			f.accts[i].WebhookSubscriptionID = sub
		}
	}
	return nil
}

func lookupReply(t *testing.T, b []byte) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func lookupAcct(athlete, sub string) domain.ExternalAccount {
	return domain.ExternalAccount{
		ID: domain.ExternalAccountID(uuid.New()), UserID: domain.UserID(uuid.New()),
		ProviderAccountID: athlete, WebhookSubscriptionID: sub, AutoImportEnabled: true,
	}
}

func TestAccountLookup(t *testing.T) {
	ctx := context.Background()
	acct := lookupAcct("7", "")
	f := &lookupAccounts{accts: []domain.ExternalAccount{acct}}
	l := &AccountLookup{Accounts: f}

	got := lookupReply(t, l.Respond(ctx, []byte(`{"provider":"strava","external_id":"7","subscription_id":"s1"}`)))
	if got["account_id"] != acct.ID.String() || f.registered[acct.ID] != "s1" {
		t.Errorf("fallback reply = %v, registered %v", got, f.registered)
	}
	if got := lookupReply(t, l.Respond(ctx, []byte(`{"provider":"strava","subscription_id":"s1"}`))); got["account_id"] != acct.ID.String() {
		t.Errorf("subscription-only = %v", got)
	}
	if got := lookupReply(t, l.Respond(ctx, []byte(`{"provider":"strava","external_id":"8"}`))); got["error"] != "not_found" {
		t.Errorf("unknown = %v", got)
	}
	if got := lookupReply(t, l.Respond(ctx, []byte(`{"provider":"strava"}`))); got["error"] == "" {
		t.Errorf("missing ids = %v", got)
	}
	got = lookupReply(t, l.RespondPool(ctx, "cairn.accounts.lookup.strava.alt", []byte(`{"provider":"evil","worker_name":"x","external_id":"7"}`)))
	if got["account_id"] == "" || f.pool != "alt" {
		t.Errorf("pool reply = %v, pool %q", got, f.pool)
	}
	f.accts[0].AutoImportEnabled = false
	if got := lookupReply(t, l.Respond(ctx, []byte(`{"provider":"strava","external_id":"7"}`))); got["error"] != "auto_import_suspended" {
		t.Errorf("suspended = %v", got)
	}
}

// Two athletes on one Strava app share its subscription; each event must
// reach the account of its owner.
func TestAccountLookupSharedSubscription(t *testing.T) {
	ctx := context.Background()
	a, b := lookupAcct("100", "s1"), lookupAcct("200", "")
	f := &lookupAccounts{accts: []domain.ExternalAccount{a, b}}
	l := &AccountLookup{Accounts: f}

	for _, tc := range []struct {
		owner string
		want  domain.ExternalAccount
	}{{"100", a}, {"200", b}, {"200", b}, {"100", a}} {
		got := lookupReply(t, l.Respond(ctx, []byte(`{"provider":"strava","external_id":"`+tc.owner+`","subscription_id":"s1"}`)))
		if got["account_id"] != tc.want.ID.String() || got["user_id"] != tc.want.UserID.String() {
			t.Errorf("owner %s = %v, want %s", tc.owner, got, tc.want.ID)
		}
	}
	if f.registered[b.ID] != "s1" || f.registered[a.ID] != "" {
		t.Errorf("self-heal = %v", f.registered)
	}
	// Both now hold s1: a subscription-only request cannot pick.
	if got := lookupReply(t, l.Respond(ctx, []byte(`{"provider":"strava","subscription_id":"s1"}`))); got["error"] != "ambiguous" {
		t.Errorf("subscription-only = %v", got)
	}
}

func TestAccountLookupAmbiguous(t *testing.T) {
	ctx := context.Background()
	// Two users linked the same athlete on the same app.
	f := &lookupAccounts{accts: []domain.ExternalAccount{lookupAcct("100", "s1"), lookupAcct("100", "s1")}}
	l := &AccountLookup{Accounts: f}
	for _, body := range []string{
		`{"provider":"strava","external_id":"100","subscription_id":"s1"}`,
		`{"provider":"strava","external_id":"100"}`,
	} {
		if got := lookupReply(t, l.Respond(ctx, []byte(body))); got["error"] != "ambiguous" {
			t.Errorf("%s = %v", body, got)
		}
	}
	if len(f.registered) != 0 {
		t.Errorf("ambiguous lookup self-healed: %v", f.registered)
	}

	// Same athlete on two apps: the fallback skips the account bound elsewhere.
	other, fresh := lookupAcct("100", "s9"), lookupAcct("100", "")
	f.accts = []domain.ExternalAccount{other, fresh}
	got := lookupReply(t, l.Respond(ctx, []byte(`{"provider":"strava","external_id":"100","subscription_id":"s2"}`)))
	if got["account_id"] != fresh.ID.String() || f.registered[fresh.ID] != "s2" {
		t.Errorf("unbound fallback = %v, registered %v", got, f.registered)
	}
}

type hookBus struct {
	port.JobBus
	subject, msgID string
	body           []byte
	reply          []byte
	reqErr         error
}

func (b *hookBus) Publish(_ context.Context, subject, msgID string, body []byte, _ ...port.PublishOpt) error {
	b.subject, b.msgID, b.body = subject, msgID, body
	return nil
}

func (b *hookBus) Request(context.Context, string, []byte, time.Duration) ([]byte, error) {
	return b.reply, b.reqErr
}

func TestWebhookForwarder(t *testing.T) {
	ctx := context.Background()
	bus := &hookBus{}
	f := &WebhookForwarder{Bus: bus, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	if err := f.Forward(ctx, "strava", map[string]string{"x-sig": "1"}, "1.2.3.4:5", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	var env webhookEventEnvelope
	_ = json.Unmarshal(bus.body, &env)
	if bus.subject != "cairn.webhooks.strava.event" || bus.msgID != "strava:2026-01-01T00:00:00Z" ||
		env.Headers["x-sig"] != "1" || env.Headers["x-webhook-source-addr"] != "1.2.3.4:5" || string(env.Body) != "hi" {
		t.Errorf("forwarded %s %s %+v", bus.subject, bus.msgID, env)
	}

	bus.reply = []byte(`{"body":"b2s="}`)
	resp, err := f.Verify(ctx, "strava", "GET", nil)
	if err != nil || resp.Status != 200 || resp.ContentType != "application/json" || string(resp.Body) != "ok" {
		t.Errorf("verify = %+v, %v", resp, err)
	}
	bus.reqErr = errors.New("no responders")
	if _, err := f.Verify(ctx, "strava", "GET", nil); !errors.Is(err, ErrWebhookNoWorker) {
		t.Errorf("no worker = %v", err)
	}
	if ValidWebhookProvider("a.b") || !ValidWebhookProvider("strava_2") {
		t.Error("provider validation")
	}
}
