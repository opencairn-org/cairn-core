package sync

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// impAccounts records config patches and pool assignments.
type impAccounts struct {
	port.ExternalAccountRepo
	acct    domain.ExternalAccount
	others  []domain.ExternalAccount
	patches []map[string]any
	assign  error
	pinned  string
}

func (f *impAccounts) GetExternalAccount(context.Context, domain.ExternalAccountID) (domain.ExternalAccount, error) {
	return f.acct, nil
}

func (f *impAccounts) UpdateAccountConfig(_ context.Context, _ domain.ExternalAccountID, p map[string]any) error {
	f.patches = append(f.patches, p)
	return nil
}

func (f *impAccounts) ListAccountsForUser(context.Context, domain.UserID) ([]domain.ExternalAccount, error) {
	return append([]domain.ExternalAccount{f.acct}, f.others...), nil
}

func (f *impAccounts) SetAssignedWorker(_ context.Context, _ domain.ExternalAccountID, name string) error {
	if f.assign != nil {
		return f.assign
	}
	f.pinned = name
	return nil
}

type impSources struct {
	port.AccountSourceRepo
	existing map[string]struct{}
	workers  []domain.SourceWorker
	outdated []domain.ActivitySource
	marked   string
}

func (f *impSources) ExistingExternalIDs(context.Context, string, domain.ExternalAccountID) (map[string]struct{}, error) {
	return f.existing, nil
}

func (f *impSources) ListSourceWorkersForAccount(context.Context, domain.ExternalAccountID) ([]domain.SourceWorker, error) {
	return f.workers, nil
}

func (f *impSources) MarkSourcesPoolChanged(_ context.Context, _ domain.ExternalAccountID, pool string) (int, error) {
	f.marked = pool
	return 3, nil
}

func (f *impSources) ListSourcesOutOfDate(context.Context, domain.ExternalAccountID, string, int) ([]domain.ActivitySource, error) {
	return f.outdated, nil
}

func (f *impSources) CountSourcesOutOfDate(context.Context, domain.ExternalAccountID) (map[string]int, error) {
	return map[string]int{"newer_parser": 7}, nil
}

type impQueue struct {
	port.ImportQueueProcessor
	port.ImportQueueAccountView
	enqueued, requeued []domain.ImportQueueItem
	inProgress         int
	claimable          []domain.ImportQueueItem
	claimedLimit       int
}

func (f *impQueue) Enqueue(_ context.Context, items []domain.ImportQueueItem) (int, error) {
	f.enqueued = append(f.enqueued, items...)
	return len(items), nil
}

func (f *impQueue) Requeue(_ context.Context, items []domain.ImportQueueItem) (int, error) {
	f.requeued = append(f.requeued, items...)
	return len(items), nil
}

func (f *impQueue) CountByStatus(context.Context, domain.ExternalAccountID) (map[domain.ImportItemStatus]int, error) {
	return map[domain.ImportItemStatus]int{domain.ImportStatusInProgress: f.inProgress}, nil
}

func (f *impQueue) ClaimPending(_ context.Context, _ domain.ExternalAccountID, limit int) ([]domain.ImportQueueItem, error) {
	f.claimedLimit = limit
	return f.claimable, nil
}

// impBus answers discover requests page by page and records publishes.
type impBus struct {
	port.JobBus
	pages     map[int]discoverResp
	reqErr    error
	published []string
}

func (b *impBus) Request(_ context.Context, _ string, body []byte, _ time.Duration) ([]byte, error) {
	if b.reqErr != nil {
		return nil, b.reqErr
	}
	var req struct {
		StartPage int `json:"start_page"`
	}
	_ = json.Unmarshal(body, &req)
	return json.Marshal(b.pages[req.StartPage])
}

func (b *impBus) Publish(_ context.Context, subject, msgID string, _ []byte, _ ...port.PublishOpt) error {
	b.published = append(b.published, subject+" "+msgID)
	return nil
}

type impPools struct {
	port.WorkerPoolRepo
	pkg string
}

func (p impPools) Get(_ context.Context, _, name string) (domain.WorkerPool, error) {
	return domain.WorkerPool{Name: name, Package: p.pkg}, nil
}

type impInsight struct {
	port.SyncInsight
	names []string
}

func (i impInsight) Pools(context.Context, string) []domain.WorkerPoolInfo {
	out := []domain.WorkerPoolInfo{}
	for _, n := range i.names {
		out = append(out, domain.WorkerPoolInfo{Name: n})
	}
	return out
}

type impReimporter struct {
	port.SourceReimporter
	n int
}

func (r *impReimporter) Reparse(context.Context, domain.UserID, domain.ActivitySource) (string, error) {
	r.n++
	return "job", nil
}

func impFixture() (*Imports, *impAccounts, *impSources, *impQueue, *impBus) {
	acct := domain.ExternalAccount{
		ID: domain.ExternalAccountID(uuid.New()), UserID: domain.UserID(uuid.New()),
		Provider: "strava", ProviderAccountID: "42",
	}
	accts := &impAccounts{acct: acct}
	srcs := &impSources{existing: map[string]struct{}{"a": {}}}
	q := &impQueue{}
	bus := &impBus{pages: map[int]discoverResp{}}
	uc := &Imports{
		Accounts: accts, Sources: srcs, Queue: q, Bus: bus, Router: staticRouter("main"),
		Pools: impPools{pkg: "strava-go"}, Insight: impInsight{names: []string{"main", "alt"}},
		Reimporter: &impReimporter{},
		Go:         func(_ string, fn func()) { fn() },
		Now:        func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	return uc, accts, srcs, q, bus
}

func TestPreview_CountsAndPersistsTotal(t *testing.T) {
	uc, accts, _, _, bus := impFixture()
	bus.pages[1] = discoverResp{Items: []discoverItem{{ExternalID: "a"}, {ExternalID: "b"}}, NextPage: 2}
	bus.pages[2] = discoverResp{Items: []discoverItem{{ExternalID: "c"}}, Complete: true}

	p, err := uc.Preview(context.Background(), accts.acct)
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 3 || p.AlreadyPresent != 1 || !p.Complete {
		t.Errorf("preview = %+v", p)
	}
	last := accts.patches[len(accts.patches)-1]
	if last["last_discovered_total"] != 3 || last["rate_limited_until"] != "" {
		t.Errorf("patch = %v", last)
	}
}

func TestPreview_RateLimitMarksAccount(t *testing.T) {
	uc, accts, _, _, bus := impFixture()
	bus.reqErr = errors.New("worker: budget exhausted")
	_, err := uc.Preview(context.Background(), accts.acct)
	var rl *RateLimitedError
	if !errors.As(err, &rl) || !rl.Until.Equal(uc.Now().Add(RateLimitCooldown)) {
		t.Fatalf("err = %v", err)
	}
	if len(accts.patches) != 1 || accts.patches[0]["rate_limited_until"] == "" {
		t.Errorf("patches = %v", accts.patches)
	}

	bus.reqErr = errors.New("nats: timeout")
	if _, err := uc.Preview(context.Background(), accts.acct); err == nil || errors.As(err, &rl) {
		t.Errorf("plain failure = %v", err)
	}
}

func TestRunFullDiscovery_EnqueuesEveryPage(t *testing.T) {
	uc, accts, _, q, bus := impFixture()
	bus.pages[1] = discoverResp{Items: []discoverItem{{ExternalID: "a", ItemType: "activity"}}, NextPage: 2}
	bus.pages[2] = discoverResp{Items: []discoverItem{{ExternalID: "b", ItemType: "activity", ItemTime: "2026-01-01T00:00:00Z"}}, Complete: true}

	uc.RunFullDiscovery(accts.acct, true)
	if len(q.enqueued) != 1 || q.enqueued[0].ExternalID != "b" || q.enqueued[0].ItemTime == nil {
		t.Errorf("skip-existing enqueued = %+v", q.enqueued)
	}
	if got := accts.patches[len(accts.patches)-1]; got["discovering"] != false {
		t.Errorf("final patch = %v", got)
	}

	uc.RunFullDiscovery(accts.acct, false)
	if len(q.requeued) != 2 {
		t.Errorf("re-download requeued = %d, want 2", len(q.requeued))
	}
}

func TestSetWorker_Rules(t *testing.T) {
	ctx := context.Background()
	uc, accts, srcs, _, _ := impFixture()

	if _, err := uc.SetWorker(ctx, accts.acct, "ghost", false); !isKind(err, port.ActionErrorInvalid) {
		t.Errorf("unknown pool: %v", err)
	}
	srcs.workers = []domain.SourceWorker{{Name: "main", Package: "strava-py"}}
	if _, err := uc.SetWorker(ctx, accts.acct, "alt", false); !isKind(err, port.ActionErrorConflict) || !strings.Contains(err.Error(), "strava-py") {
		t.Errorf("package change unconfirmed: %v", err)
	}
	n, err := uc.SetWorker(ctx, accts.acct, "alt", true)
	if err != nil || n != 3 || srcs.marked != "alt" || accts.pinned != "alt" {
		t.Errorf("confirmed move = %d, %v (marked %q, pinned %q)", n, err, srcs.marked, accts.pinned)
	}

	accts.others = []domain.ExternalAccount{{ID: domain.ExternalAccountID(uuid.New()), Provider: "strava", ProviderAccountID: "42"}}
	if _, err := uc.SetWorker(ctx, accts.acct, "", false); !isKind(err, port.ActionErrorConflict) {
		t.Errorf("unpin with sibling: %v", err)
	}
	accts.others = nil
	accts.assign = domain.ErrPoolTaken
	srcs.workers = nil
	if _, err := uc.SetWorker(ctx, accts.acct, "main", false); !isKind(err, port.ActionErrorConflict) {
		t.Errorf("pool taken: %v", err)
	}
}

func isKind(err error, k port.ActionErrorKind) bool {
	var ae *port.ActionError
	return errors.As(err, &ae) && ae.Kind == k
}

func TestReparseOutOfDate_SkipsWithoutArchive(t *testing.T) {
	uc, accts, srcs, _, _ := impFixture()
	srcs.outdated = []domain.ActivitySource{{RawBlobID: "x"}, {}, {RawBlobID: "y"}}
	res, err := uc.ReparseOutOfDate(context.Background(), accts.acct)
	if err != nil || res != (Reparse{Queued: 2, Skipped: 1, Remaining: 7}) {
		t.Errorf("reparse = %+v, %v", res, err)
	}
}

func TestDispatchAccount_RespectsCapacity(t *testing.T) {
	uc, accts, _, q, bus := impFixture()
	q.inProgress = 3
	q.claimable = []domain.ImportQueueItem{{ID: domain.ImportQueueItemID(uuid.New()), Provider: "strava", ExternalID: "9"}}
	uc.DispatchAccount(context.Background(), uc.log(), accts.acct.ID)
	if q.claimedLimit != 2 {
		t.Errorf("claimed limit = %d, want 2", q.claimedLimit)
	}
	want := domain.JobSubject("fetch_source", "strava", "main") + " queue:strava:" + accts.acct.ID.String() + ":9"
	if len(bus.published) != 1 || bus.published[0] != want {
		t.Errorf("published = %v", bus.published)
	}

	q.inProgress, q.claimedLimit = 5, 0
	uc.DispatchAccount(context.Background(), uc.log(), accts.acct.ID)
	if q.claimedLimit != 0 {
		t.Error("claimed beyond capacity")
	}
}
