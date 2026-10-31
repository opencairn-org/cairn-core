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

// lookupAccounts resolves by subscription and records self-heals.
type lookupAccounts struct {
	port.ExternalAccountRepo
	acct       domain.ExternalAccount
	bySub      bool
	registered string
	pool       string
}

func (f *lookupAccounts) FindBySubscription(_ context.Context, _, _, pool string) (domain.ExternalAccount, error) {
	f.pool = pool
	if !f.bySub {
		return domain.ExternalAccount{}, domain.ErrNotFound
	}
	return f.acct, nil
}

func (f *lookupAccounts) FindByProviderAndExternalID(_ context.Context, _, ext, pool string) (domain.ExternalAccount, error) {
	f.pool = pool
	if ext != f.acct.ProviderAccountID {
		return domain.ExternalAccount{}, domain.ErrNotFound
	}
	return f.acct, nil
}

func (f *lookupAccounts) RegisterWebhookSubscription(_ context.Context, _, sub string, _ domain.ExternalAccountID) error {
	f.registered = sub
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

func TestAccountLookup(t *testing.T) {
	ctx := context.Background()
	acct := domain.ExternalAccount{ID: domain.ExternalAccountID(uuid.New()), UserID: domain.UserID(uuid.New()), ProviderAccountID: "7", AutoImportEnabled: true}
	f := &lookupAccounts{acct: acct}
	l := &AccountLookup{Accounts: f}

	got := lookupReply(t, l.Respond(ctx, []byte(`{"provider":"strava","external_id":"7","subscription_id":"s1"}`)))
	if got["account_id"] != acct.ID.String() || f.registered != "s1" {
		t.Errorf("fallback reply = %v, registered %q", got, f.registered)
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
	f.acct.AutoImportEnabled = false
	if got := lookupReply(t, l.Respond(ctx, []byte(`{"provider":"strava","external_id":"7"}`))); got["error"] != "auto_import_suspended" {
		t.Errorf("suspended = %v", got)
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
