package connect

import (
	"context"
	"sync"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	notificationuc "github.com/opencairn-org/cairn-core/internal/usecase/notification"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

type fakeNotifRepo struct {
	port.NotificationRepo
	unread int
}

func (f *fakeNotifRepo) CountUnreadForUser(context.Context, domain.UserID) (int, error) {
	return f.unread, nil
}
func (f *fakeNotifRepo) SaveNotifications(context.Context, []domain.Notification) error { return nil }

// fakeStream records what the handler sends.
type fakeStream struct {
	mu   sync.Mutex
	sent []*v1.NotificationEvent
}

func (f *fakeStream) Send(e *v1.NotificationEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, e)
	return nil
}
func (f *fakeStream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func TestCountUnreadAndStreamNotifications(t *testing.T) {
	user := domain.UserID(uuid.New())
	hub := notificationuc.NewHub()
	repo := notificationuc.NewBroadcastRepo(&fakeNotifRepo{unread: 3}, hub)
	s := NewNotificationServer(repo)
	s.Hub = hub
	ctx := context.WithValue(t.Context(), ctxKeyUserID, user)

	cnt, err := s.CountUnreadNotifications(ctx, &v1.CountUnreadNotificationsRequest{})
	if err != nil || cnt.GetCount() != 3 {
		t.Fatalf("count %v %v", cnt, err)
	}

	sctx, cancel := context.WithCancel(ctx)
	stream := &fakeStream{}
	done := make(chan error, 1)
	go func() {
		done <- s.streamNotifications(sctx, &v1.StreamNotificationsRequest{MinSeverity: v1.NotificationSeverity_NOTIFICATION_SEVERITY_WARN}, stream)
	}()
	// Wait for the subscription, then save: the warn event streams, the info one is filtered.
	for i := 0; i < 100 && hub.Subscribers(user) == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if err := repo.SaveNotifications(ctx, []domain.Notification{
		{UserID: user, Severity: domain.NotificationSeverityInfo, DedupKey: "quiet"},
		{UserID: user, Severity: domain.NotificationSeverityError, DedupKey: "loud"},
		{UserID: domain.UserID(uuid.New()), Severity: domain.NotificationSeverityError, DedupKey: "other"},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && stream.count() == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("stream ended with %v", err)
	}
	if stream.count() != 1 || stream.sent[0].GetDedupKey() != "loud" {
		t.Fatalf("sent %v", stream.sent)
	}
	if hub.Subscribers(user) != 0 {
		t.Fatal("stream must unsubscribe on exit")
	}
	s.Hub = nil
	if err := s.streamNotifications(ctx, &v1.StreamNotificationsRequest{}, &fakeStream{}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("no hub: %v", err)
	}
	if err := s.streamNotifications(t.Context(), &v1.StreamNotificationsRequest{}, &fakeStream{}); connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
}

type fakeAcctRepo struct {
	port.ExternalAccountRepo
	accts   []domain.ExternalAccount
	auto    map[domain.ExternalAccountID]bool
	patches map[domain.ExternalAccountID]map[string]any
}

func (f *fakeAcctRepo) ListAccountsForUser(_ context.Context, uid domain.UserID) ([]domain.ExternalAccount, error) {
	var out []domain.ExternalAccount
	for _, a := range f.accts {
		if a.UserID == uid {
			out = append(out, a)
		}
	}
	return out, nil
}
func (f *fakeAcctRepo) GetExternalAccount(_ context.Context, id domain.ExternalAccountID) (domain.ExternalAccount, error) {
	for _, a := range f.accts {
		if a.ID == id {
			return a, nil
		}
	}
	return domain.ExternalAccount{}, domain.ErrNotFound
}
func (f *fakeAcctRepo) SetAutoImport(_ context.Context, id domain.ExternalAccountID, on bool) error {
	f.auto[id] = on
	return nil
}
func (f *fakeAcctRepo) UpdateAccountConfig(_ context.Context, id domain.ExternalAccountID, patch map[string]any) error {
	f.patches[id] = patch
	return nil
}

type fakeInsightC struct{ synced []domain.ExternalAccountID }

func (f *fakeInsightC) Budget(context.Context, domain.ExternalAccount) domain.RateBudget {
	return domain.RateBudget{}
}
func (f *fakeInsightC) Pools(context.Context, string) []domain.WorkerPoolInfo {
	return []domain.WorkerPoolInfo{{Name: "p", Package: "strava", Online: true, Compatible: true}}
}
func (f *fakeInsightC) Providers() []domain.ProviderInfo {
	return []domain.ProviderInfo{{ID: "strava", DisplayName: "Strava", OAuth: true}}
}
func (f *fakeInsightC) ActivityURL(string, string) string { return "" }
func (f *fakeInsightC) StartFullSync(a domain.ExternalAccount, _ bool) {
	f.synced = append(f.synced, a.ID)
}

type acctActivities struct{ *inmem.ActivityRepo }

func (acctActivities) ExistingExternalIDs(context.Context, string, domain.ExternalAccountID) (map[string]struct{}, error) {
	return map[string]struct{}{"x": {}}, nil
}
func (acctActivities) CountSourcesOutOfDate(context.Context, domain.ExternalAccountID) (map[string]int, error) {
	return nil, nil
}

func TestExternalAccountServer(t *testing.T) {
	user := domain.UserID(uuid.New())
	acct := domain.ExternalAccount{ID: domain.ExternalAccountID(uuid.New()), UserID: user, Provider: "strava", Status: domain.ExternalAccountStatusActive, AutoImportEnabled: true}
	repo := &fakeAcctRepo{accts: []domain.ExternalAccount{acct}, auto: map[domain.ExternalAccountID]bool{}, patches: map[domain.ExternalAccountID]map[string]any{}}
	insight := &fakeInsightC{}
	accounts := view.NewAccounts(repo, acctActivities{inmem.NewActivityRepo()}, nil, nil, nil, insight, nil)
	s := NewExternalAccountServer(accounts, repo, insight, 900)
	ctx := context.WithValue(t.Context(), ctxKeyUserID, user)

	list, err := s.ListExternalAccounts(ctx, &v1.ListExternalAccountsRequest{})
	if err != nil || len(list.GetAccounts()) != 1 || list.GetDefaultPollIntervalSeconds() != 900 {
		t.Fatalf("list %v %v", list, err)
	}
	a := list.GetAccounts()[0]
	if a.GetImported() != 1 || a.GetStatus() != v1.ExternalAccountStatus_EXTERNAL_ACCOUNT_STATUS_ACTIVE || len(a.GetWorkerPools()) != 1 || !a.GetAutoImportEnabled() || a.GetQueue() == nil {
		t.Fatalf("account %v", a)
	}
	prov, _ := s.ListAvailableProviders(ctx, &v1.ListAvailableProvidersRequest{})
	if len(prov.GetProviders()) != 1 || prov.GetProviders()[0].GetAuthMethod() != v1.AvailableAuthMethod_AVAILABLE_AUTH_METHOD_OAUTH2 {
		t.Fatalf("providers %v", prov)
	}
	if res, err := s.SetExternalAccountEnabled(ctx, &v1.SetExternalAccountEnabledRequest{Id: acct.ID.String(), Enabled: false}); err != nil || repo.auto[acct.ID] || res.GetAccount().GetId() != acct.ID.String() {
		t.Fatalf("set enabled %v %v", res, err)
	}
	if _, err := s.UpdateExternalAccountConfig(ctx, &v1.UpdateExternalAccountConfigRequest{Id: acct.ID.String(), Config: map[string]string{"poll_interval_seconds": "300"}}); err != nil || repo.patches[acct.ID]["poll_interval_seconds"] != 300 {
		t.Fatalf("config %v %v", repo.patches, err)
	}
	_, err = s.UpdateExternalAccountConfig(ctx, &v1.UpdateExternalAccountConfigRequest{Id: acct.ID.String(), Config: map[string]string{"other": "1"}})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	w := "p"
	_, err = s.UpdateExternalAccountConfig(ctx, &v1.UpdateExternalAccountConfigRequest{Id: acct.ID.String(), AssignedWorkerName: &w})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	if res, err := s.TriggerSync(ctx, &v1.TriggerSyncRequest{Id: acct.ID.String()}); err != nil || len(insight.synced) != 1 || res.GetJobId() == "" {
		t.Fatalf("trigger %v %v", res, err)
	}
	other := context.WithValue(t.Context(), ctxKeyUserID, domain.UserID(uuid.New()))
	_, err = s.TriggerSync(other, &v1.TriggerSyncRequest{Id: acct.ID.String()})
	wantCode(t, err, connectrpc.CodeNotFound)
	_, err = s.ListSyncHistory(ctx, &v1.ListSyncHistoryRequest{AccountId: "nope"})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	_, err = s.ListQueueItems(ctx, &v1.ListQueueItemsRequest{AccountId: acct.ID.String()})
	wantCode(t, err, connectrpc.CodeUnavailable)
}
