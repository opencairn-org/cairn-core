package view

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeAccounts struct {
	port.ExternalAccountRepo
	accts []domain.ExternalAccount
}

func (f *fakeAccounts) ListAccountsForUser(_ context.Context, uid domain.UserID) ([]domain.ExternalAccount, error) {
	var out []domain.ExternalAccount
	for _, a := range f.accts {
		if a.UserID == uid {
			out = append(out, a)
		}
	}
	return out, nil
}
func (f *fakeAccounts) GetExternalAccount(_ context.Context, id domain.ExternalAccountID) (domain.ExternalAccount, error) {
	for _, a := range f.accts {
		if a.ID == id {
			return a, nil
		}
	}
	return domain.ExternalAccount{}, domain.ErrNotFound
}

type fakeQueue struct {
	port.ImportQueueRepo
	items []domain.ImportQueueItem
}

func (f *fakeQueue) CountByStatus(context.Context, domain.ExternalAccountID) (map[domain.ImportItemStatus]int, error) {
	out := map[domain.ImportItemStatus]int{}
	for _, it := range f.items {
		out[it.Status]++
	}
	return out, nil
}
func (f *fakeQueue) ListForAccount(_ context.Context, _ domain.ExternalAccountID, statuses []domain.ImportItemStatus, limit int) ([]domain.ImportQueueItem, error) {
	var out []domain.ImportQueueItem
	for _, it := range f.items {
		keep := len(statuses) == 0
		for _, s := range statuses {
			if s == it.Status {
				keep = true
			}
		}
		if keep && len(out) < limit {
			out = append(out, it)
		}
	}
	return out, nil
}

type fakeEvents struct {
	port.ImportEventRepo
	events []domain.ConnectionImportEvent
}

func (f *fakeEvents) ListForAccount(context.Context, domain.ExternalAccountID, int) ([]domain.ConnectionImportEvent, error) {
	return f.events, nil
}

type fakeInsight struct {
	budget domain.RateBudget
	synced []domain.ExternalAccountID
}

func (f *fakeInsight) Budget(context.Context, domain.ExternalAccount) domain.RateBudget {
	return f.budget
}
func (f *fakeInsight) Pools(context.Context, string) []domain.WorkerPoolInfo {
	return []domain.WorkerPoolInfo{{Name: "strava-main", Package: "strava", Online: true, Compatible: true}}
}
func (f *fakeInsight) Providers() []domain.ProviderInfo {
	return []domain.ProviderInfo{{ID: "garmin", DisplayName: "Garmin"}, {ID: "strava", DisplayName: "Strava", OAuth: true}}
}
func (f *fakeInsight) ActivityURL(provider, id string) string {
	if provider == "strava" {
		return "https://www.strava.com/activities/" + id
	}
	return ""
}
func (f *fakeInsight) StartFullSync(a domain.ExternalAccount, _ bool) {
	f.synced = append(f.synced, a.ID)
}

type fakeConns struct {
	port.UserProviderConfigRepo
	conns []domain.UserProviderConfig
}

func (f *fakeConns) ListForUser(context.Context, domain.UserID) ([]domain.UserProviderConfig, error) {
	return f.conns, nil
}

// outOfDateRepo adds the account-scoped counts the inmem repo lacks.
type outOfDateRepo struct {
	*inmem.ActivityRepo
	src domain.ActivitySource
}

func (r *outOfDateRepo) ExistingExternalIDs(context.Context, string, domain.ExternalAccountID) (map[string]struct{}, error) {
	return map[string]struct{}{"a": {}, "b": {}}, nil
}
func (r *outOfDateRepo) CountSourcesOutOfDate(context.Context, domain.ExternalAccountID) (map[string]int, error) {
	return map[string]int{"newer_parser": 3, "pool_changed": 1}, nil
}
func (r *outOfDateRepo) FindSourceByExternalID(_ context.Context, _ string, _ *domain.ExternalAccountID, ext string) (domain.ActivitySource, error) {
	if ext == "42" {
		return r.src, nil
	}
	return domain.ActivitySource{}, domain.ErrNotFound
}

func TestAccountsListAndOwned(t *testing.T) {
	user := domain.UserID(uuid.New())
	acct := domain.ExternalAccount{ID: domain.ExternalAccountID(uuid.New()), UserID: user, Provider: "strava", AssignedWorkerName: "strava-main",
		Config:    json.RawMessage(`{"poll_interval_seconds": 600, "last_discovered_total": 1, "discovering": true}`),
		RateLimit: &domain.RateLimitSnapshot{ShortWindowLimit: 100, ShortWindowUsed: 100, ShortWindowResetAt: vNow.Add(time.Hour)}}
	other := domain.ExternalAccount{ID: domain.ExternalAccountID(uuid.New()), UserID: domain.UserID(uuid.New()), Provider: "garmin"}
	repo := &outOfDateRepo{ActivityRepo: inmem.NewActivityRepo()}
	queue := &fakeQueue{items: []domain.ImportQueueItem{{Status: domain.ImportStatusPending}, {Status: domain.ImportStatusDone}, {Status: domain.ImportStatusDone}}}
	insight := &fakeInsight{budget: domain.RateBudget{Short: &domain.BudgetWindow{Used: 10, Limit: 100, ResetsAt: vNow.Add(time.Hour)}}}
	a := NewAccounts(&fakeAccounts{accts: []domain.ExternalAccount{acct, other}}, repo, queue, nil, nil, insight, func() time.Time { return vNow })

	rows, err := a.List(context.Background(), user)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v %v", rows, err)
	}
	r := rows[0]
	if r.Imported != 2 || r.ProviderTotal != 2 || !r.Discovering || r.PollInterval != 600 || !r.RateLimited || r.OutOfDate != 4 {
		t.Fatalf("row %+v", r)
	}
	if r.Queue[domain.ImportStatusDone] != 2 || len(r.Pools) != 1 || r.Budget.Short == nil {
		t.Fatalf("queue/pools/budget %+v", r)
	}
	if _, err := a.Owned(context.Background(), user, other.ID); err == nil {
		t.Fatal("foreign account must be not found")
	}
	if got, err := a.Owned(context.Background(), user, acct.ID); err != nil || got.ID != acct.ID {
		t.Fatalf("owned %v %v", got, err)
	}
}

func TestAccountsHistoryQueueConnections(t *testing.T) {
	user := domain.UserID(uuid.New())
	acct := domain.ExternalAccount{ID: domain.ExternalAccountID(uuid.New()), UserID: user, Provider: "strava"}
	aid := domain.ActivityID(uuid.New())
	repo := &outOfDateRepo{ActivityRepo: inmem.NewActivityRepo(), src: domain.ActivitySource{ActivityID: aid}}
	events := &fakeEvents{events: []domain.ConnectionImportEvent{
		{Kind: domain.ImportEventActivityImported, ExternalID: "42"},
		{Kind: domain.ImportEventActivityImported, ExternalID: "43"},
		{Kind: domain.ImportEventFailed, ExternalID: "44"},
	}}
	queue := &fakeQueue{items: []domain.ImportQueueItem{{ExternalID: "1", Status: domain.ImportStatusPending}, {ExternalID: "2", Status: domain.ImportStatusDone}}}
	conns := &fakeConns{conns: []domain.UserProviderConfig{{Provider: "strava", Label: "main"}, {Provider: "unknown"}}}
	a := NewAccounts(&fakeAccounts{accts: []domain.ExternalAccount{acct}}, repo, queue, events, conns, &fakeInsight{}, nil)

	hist, err := a.History(context.Background(), acct, 0)
	if err != nil || len(hist) != 3 {
		t.Fatalf("history %v %v", hist, err)
	}
	if hist[0].Status != "completed" || hist[0].ActivityID == nil || *hist[0].ActivityID != aid || hist[0].ExternalURL == "" {
		t.Fatalf("linked event %+v", hist[0])
	}
	if hist[1].Status != "ongoing" || hist[2].Status != "failed" {
		t.Fatalf("statuses %+v %+v", hist[1], hist[2])
	}
	items, err := a.QueueItems(context.Background(), acct, DefaultQueueStatuses, 0)
	if err != nil || len(items) != 1 || items[0].Item.ExternalID != "1" || items[0].ExternalURL == "" {
		t.Fatalf("queue %v %v", items, err)
	}
	all, _ := a.QueueItems(context.Background(), acct, nil, 10)
	if len(all) != 2 {
		t.Fatalf("all statuses %d", len(all))
	}
	rows, providers, err := a.Connections(context.Background(), user)
	if err != nil || len(rows) != 2 || rows[0].DisplayName != "Strava" || !rows[0].OAuth || rows[1].DisplayName != "unknown" || len(providers) != 2 {
		t.Fatalf("connections %v %v %v", rows, providers, err)
	}
}

func TestParseAccountConfigAndRateLimited(t *testing.T) {
	v := domain.ParseAccountConfig(json.RawMessage(`{"rate_limited_until":"`+vNow.Add(time.Hour).Format(time.RFC3339)+`"}`), vNow)
	if !v.RateLimited {
		t.Fatal("future cooldown must count as rate limited")
	}
	if domain.ParseAccountConfig(json.RawMessage(`{"rate_limited_until":"`+vNow.Add(-time.Hour).Format(time.RFC3339)+`"}`), vNow).RateLimited {
		t.Fatal("elapsed cooldown must not")
	}
	if domain.ParseAccountConfig(nil, vNow) != (domain.AccountConfigView{}) || domain.ParseAccountConfig(json.RawMessage(`nope`), vNow) != (domain.AccountConfigView{}) {
		t.Fatal("empty or invalid config → zero view")
	}
	rl := &domain.RateLimitSnapshot{LongWindowLimit: 10, LongWindowUsed: 10, LongWindowResetAt: vNow.Add(time.Minute)}
	if !domain.RateLimitedNow(rl, vNow) || domain.RateLimitedNow(rl, vNow.Add(2*time.Minute)) || domain.RateLimitedNow(nil, vNow) {
		t.Fatal("RateLimitedNow wrong")
	}
	b := domain.RateBudget{Daily: &domain.BudgetWindow{Used: 5, Limit: 5, ResetsAt: vNow.Add(time.Hour)}}
	if !b.Exhausted(vNow) || b.Exhausted(vNow.Add(2*time.Hour)) || (domain.RateBudget{}).Exhausted(vNow) {
		t.Fatal("Exhausted wrong")
	}
}
