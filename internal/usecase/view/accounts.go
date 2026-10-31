package view

import (
	"context"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// AccountRow is one provider account with its sync status
// (ExternalAccountService.ListExternalAccounts, GET /api/accounts).
type AccountRow struct {
	Account       domain.ExternalAccount
	Imported      int
	ProviderTotal int
	Discovering   bool
	RateLimited   bool
	PollInterval  int
	Budget        domain.RateBudget
	Queue         map[domain.ImportItemStatus]int
	Pools         []domain.WorkerPoolInfo
	OutOfDate     int
}

// SyncEventRow is one import-history event with its links.
type SyncEventRow struct {
	Event       domain.ConnectionImportEvent
	ActivityID  *domain.ActivityID
	ExternalURL string
	Status      string // failed | completed | ongoing
}

// QueueItemRow is one queue item with its provider link.
type QueueItemRow struct {
	Item        domain.ImportQueueItem
	ExternalURL string
}

// ConnectionRow is a provider connection with its display name.
type ConnectionRow struct {
	Connection  domain.UserProviderConfig
	DisplayName string
	OAuth       bool
}

// ProviderRow is a connectable provider with its pools.
type ProviderRow struct {
	Provider domain.ProviderInfo
	Pools    []domain.WorkerPoolInfo
}

// Accounts are the provider-account reads shared by REST and Connect.
type Accounts struct {
	accounts    port.ExternalAccountRepo
	activities  port.SourceReader
	queue       port.ImportQueueAccountView
	events      port.ImportEventRepo
	connections port.UserProviderConfigRepo
	insight     port.SyncInsight
	now         func() time.Time
}

// NewAccounts wires the reads; queue, events, connections and insight may be nil.
func NewAccounts(accounts port.ExternalAccountRepo, activities port.SourceReader, queue port.ImportQueueAccountView, events port.ImportEventRepo,
	connections port.UserProviderConfigRepo, insight port.SyncInsight, now func() time.Time) *Accounts {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Accounts{accounts: accounts, activities: activities, queue: queue, events: events, connections: connections, insight: insight, now: now}
}

// Owned loads the caller's account; domain.ErrNotFound for anyone else's.
func (a *Accounts) Owned(ctx context.Context, userID domain.UserID, id domain.ExternalAccountID) (domain.ExternalAccount, error) {
	acct, err := a.accounts.GetExternalAccount(ctx, id)
	if err != nil || acct.UserID != userID {
		return domain.ExternalAccount{}, domain.ErrNotFound
	}
	return acct, nil
}

// List renders every account of the user with its sync status.
func (a *Accounts) List(ctx context.Context, userID domain.UserID) ([]AccountRow, error) {
	accts, err := a.accounts.ListAccountsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	budgets := map[string]domain.RateBudget{}
	rows := make([]AccountRow, 0, len(accts))
	for _, acct := range accts {
		row := AccountRow{Account: acct}
		if a.insight != nil {
			key := acct.Provider + "/" + acct.AssignedWorkerName
			if _, seen := budgets[key]; !seen {
				budgets[key] = a.insight.Budget(ctx, acct)
			}
			row.Budget = budgets[key]
			row.Pools = a.insight.Pools(ctx, acct.Provider)
		}
		a.fillStatus(ctx, &row)
		rows = append(rows, row)
	}
	return rows, nil
}

// fillStatus adds the counts and flags of one account.
func (a *Accounts) fillStatus(ctx context.Context, row *AccountRow) {
	acct := row.Account
	if existing, err := a.activities.ExistingExternalIDs(ctx, acct.Provider, acct.ID); err == nil {
		row.Imported = len(existing)
	}
	cfg := domain.ParseAccountConfig(acct.Config, a.now())
	row.Discovering, row.PollInterval = cfg.Discovering, cfg.PollIntervalSeconds
	row.RateLimited = domain.RateLimitedNow(acct.RateLimit, a.now()) || row.Budget.Exhausted(a.now()) || cfg.RateLimited
	// last_discovered_total is a snapshot; the live imported count can
	// overtake it, so clamp instead of showing an impossible ratio. Zero
	// stays zero: never discovered.
	row.ProviderTotal = cfg.ProviderTotal
	if row.ProviderTotal > 0 && row.Imported > row.ProviderTotal {
		row.ProviderTotal = row.Imported
	}
	if a.queue != nil {
		row.Queue, _ = a.queue.CountByStatus(ctx, acct.ID)
	}
	if counts, err := a.activities.CountSourcesOutOfDate(ctx, acct.ID); err == nil {
		for _, n := range counts {
			row.OutOfDate += n
		}
	}
}

// historyLimit is how many events one history read returns.
const historyLimit = 50

// History lists the account's import events, newest first, each linked to
// the imported activity and the provider page when possible.
func (a *Accounts) History(ctx context.Context, acct domain.ExternalAccount, limit int) ([]SyncEventRow, error) {
	if a.events == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = historyLimit
	}
	list, err := a.events.ListForAccount(ctx, acct.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("list import events: %w", err)
	}
	rows := make([]SyncEventRow, 0, len(list))
	for _, e := range list {
		rows = append(rows, a.historyRow(ctx, acct, e))
	}
	return rows, nil
}

func (a *Accounts) historyRow(ctx context.Context, acct domain.ExternalAccount, e domain.ConnectionImportEvent) SyncEventRow {
	row := SyncEventRow{Event: e, Status: "ongoing"}
	if e.ExternalID != "" && (e.Kind == domain.ImportEventActivityImported || e.Kind == domain.ImportEventActivityUpdated) {
		if src, err := a.activities.FindSourceByExternalID(ctx, acct.Provider, &acct.ID, e.ExternalID); err == nil {
			id := src.ActivityID
			row.ActivityID = &id
		}
		if a.insight != nil {
			row.ExternalURL = a.insight.ActivityURL(acct.Provider, e.ExternalID)
		}
	}
	switch {
	case e.Kind == domain.ImportEventFailed:
		row.Status = "failed"
	case row.ActivityID != nil:
		row.Status = "completed"
	}
	return row
}

// DefaultQueueStatuses is the queue view without the finished rows.
var DefaultQueueStatuses = []domain.ImportItemStatus{domain.ImportStatusPending, domain.ImportStatusInProgress, domain.ImportStatusFailed}

// Queue item paging bounds.
const (
	QueueDefaultLimit = 100
	QueueMaxLimit     = 500
)

// QueueItems lists the account's queue rows in the given statuses (nil = every status).
func (a *Accounts) QueueItems(ctx context.Context, acct domain.ExternalAccount, statuses []domain.ImportItemStatus, limit int) ([]QueueItemRow, error) {
	if a.queue == nil {
		return nil, ErrUnavailable
	}
	if limit <= 0 {
		limit = QueueDefaultLimit
	}
	if limit > QueueMaxLimit {
		limit = QueueMaxLimit
	}
	items, err := a.queue.ListForAccount(ctx, acct.ID, statuses, limit)
	if err != nil {
		return nil, fmt.Errorf("list queue items: %w", err)
	}
	rows := make([]QueueItemRow, 0, len(items))
	for _, it := range items {
		row := QueueItemRow{Item: it}
		if a.insight != nil {
			row.ExternalURL = a.insight.ActivityURL(acct.Provider, it.ExternalID)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Connections lists the user's connections and the connectable providers.
func (a *Accounts) Connections(ctx context.Context, userID domain.UserID) ([]ConnectionRow, []ProviderRow, error) {
	if a.connections == nil {
		return nil, nil, ErrUnavailable
	}
	conns, err := a.connections.ListForUser(ctx, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("list connections: %w", err)
	}
	providers := map[string]domain.ProviderInfo{}
	var providerRows []ProviderRow
	if a.insight != nil {
		for _, p := range a.insight.Providers() {
			providers[p.ID] = p
			providerRows = append(providerRows, ProviderRow{Provider: p, Pools: a.insight.Pools(ctx, p.ID)})
		}
	}
	rows := make([]ConnectionRow, 0, len(conns))
	for _, c := range conns {
		row := ConnectionRow{Connection: c, DisplayName: c.Provider}
		if p, ok := providers[c.Provider]; ok {
			row.DisplayName, row.OAuth = p.DisplayName, p.OAuth
		}
		rows = append(rows, row)
	}
	return rows, providerRows, nil
}
