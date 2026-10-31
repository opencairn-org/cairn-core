package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Imports drives a connection's imports: full-sync discovery, single-item
// imports, the persisted import queue and the connection settings.
//
// Flow: preview (discover + summarize, no enqueue) → start (background
// discovery enqueues incrementally) → the queue processor drains the queue
// paced + rate-limit-aware → the result router marks each item done.
type Imports struct {
	Accounts   port.ExternalAccountRepo
	Sources    port.AccountSourceRepo
	Queue      port.ImportQueueRepo
	Events     port.ImportEventRepo // may be nil
	Bus        port.JobBus
	Router     port.WorkerRouter
	Pools      port.WorkerPoolRepo
	Insight    port.SyncInsight // known pool names
	Reimporter port.SourceReimporter
	// Go runs fn in the background; nil uses a plain goroutine.
	Go  func(name string, fn func())
	Log *slog.Logger
	Now func() time.Time
}

// RateLimitCooldown is how long a connection shows as rate-limited after the
// worker reports an exhausted budget; it gives no reset time, and a
// successful discover clears the marker early.
const RateLimitCooldown = 30 * time.Minute

// RateLimitedError is a discover refused on an exhausted provider budget.
type RateLimitedError struct{ Until time.Time }

func (e *RateLimitedError) Error() string {
	return "rate limited until " + e.Until.Format(time.RFC3339)
}

func (u *Imports) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now().UTC()
}

func (u *Imports) log() *slog.Logger {
	if u.Log != nil {
		return u.Log
	}
	return slog.Default()
}

func (u *Imports) goBackground(name string, fn func()) {
	if u.Go != nil {
		u.Go(name, fn)
		return
	}
	go fn()
}

// discoverItem is one importable item the worker reports.
type discoverItem struct {
	ItemType   string `json:"item_type"`
	ExternalID string `json:"external_id"`
	ItemTime   string `json:"item_time"`
}

type discoverResp struct {
	Items       []discoverItem `json:"items"`
	Complete    bool           `json:"complete"`
	NextPage    int            `json:"next_page,omitempty"`
	RateLimited bool           `json:"rate_limited,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// discoverBatch asks the account's worker for one batch from startPage: it
// walks a few pages and reports Complete, or NextPage + RateLimited to resume.
func (u *Imports) discoverBatch(ctx context.Context, a domain.ExternalAccount, startPage int) (discoverResp, error) {
	reqBody, _ := json.Marshal(map[string]any{"account_id": a.ID.String(), "start_page": startPage})
	name, err := u.Router.AccountWorker(ctx, a)
	if err != nil {
		return discoverResp{}, err
	}
	respBytes, err := u.Bus.Request(ctx, domain.DiscoverSubject(a.Provider, name), reqBody, 90*time.Second)
	if err != nil {
		return discoverResp{}, err
	}
	var resp discoverResp
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return discoverResp{}, err
	}
	if resp.Error != "" {
		return discoverResp{}, fmt.Errorf("worker: %s", resp.Error)
	}
	return resp, nil
}

// discoverAllNoWait walks every page without waiting on rate limits; on a
// limit it returns the partial set with complete=false (a lower bound).
func (u *Imports) discoverAllNoWait(ctx context.Context, a domain.ExternalAccount) (items []discoverItem, complete bool, err error) {
	page := 1
	for range 2000 { // safety bound on round-trips
		b, e := u.discoverBatch(ctx, a, page)
		if e != nil {
			return items, false, e
		}
		items = append(items, b.Items...)
		if b.Complete {
			return items, true, nil
		}
		if b.RateLimited {
			return items, false, nil
		}
		page = b.NextPage
		if page <= 0 {
			return items, true, nil
		}
	}
	return items, false, nil
}

// isRateLimitErr reports whether a discover error is the worker's
// provider-budget-exhausted signal rather than a real failure.
func isRateLimitErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "rate_limited") || strings.Contains(s, "budget exhausted") ||
		strings.Contains(s, "rate limited")
}

// Preview is the full-sync estimate.
type Preview struct {
	Total, AlreadyPresent int
	// Complete is false when a rate limit cut the walk short (counts are a lower bound).
	Complete bool
}

// Preview discovers without enqueueing. A rate-limit signal marks the
// connection rate-limited and returns *RateLimitedError.
func (u *Imports) Preview(ctx context.Context, a domain.ExternalAccount) (Preview, error) {
	items, complete, err := u.discoverAllNoWait(ctx, a)
	if err != nil {
		if isRateLimitErr(err) {
			until := u.now().Add(RateLimitCooldown)
			_ = u.Accounts.UpdateAccountConfig(ctx, a.ID, map[string]any{"rate_limited_until": until.Format(time.RFC3339)})
			return Preview{}, &RateLimitedError{Until: until}
		}
		return Preview{}, err
	}
	// The provider-side total backs "imported X / Y"; success clears a stale rate-limit marker.
	_ = u.Accounts.UpdateAccountConfig(ctx, a.ID, map[string]any{"last_discovered_total": len(items), "rate_limited_until": ""})
	existing, _ := u.Sources.ExistingExternalIDs(ctx, a.Provider, a.ID)
	present := 0
	for _, it := range items {
		if _, ok := existing[it.ExternalID]; ok {
			present++
		}
	}
	return Preview{Total: len(items), AlreadyPresent: present, Complete: complete}, nil
}

// StartFullSync runs the full discovery in the background.
func (u *Imports) StartFullSync(a domain.ExternalAccount, skipExisting bool) {
	u.goBackground("full discovery", func() { u.RunFullDiscovery(a, skipExisting) })
	u.log().Info("full discovery started", "account_id", a.ID, "skip_existing", skipExisting)
}

// buildImportQueue turns discovered items into queue rows, optionally
// skipping already-imported external ids.
func buildImportQueue(items []discoverItem, a domain.ExternalAccount, skipExisting bool, existing map[string]struct{}) []domain.ImportQueueItem {
	queue := make([]domain.ImportQueueItem, 0, len(items))
	for _, it := range items {
		if skipExisting {
			if _, present := existing[it.ExternalID]; present {
				continue
			}
		}
		var t *time.Time
		if it.ItemTime != "" {
			if parsed, e := time.Parse(time.RFC3339, it.ItemTime); e == nil {
				t = &parsed
			}
		}
		queue = append(queue, domain.ImportQueueItem{
			ExternalAccountID: a.ID, UserID: a.UserID, Provider: a.Provider,
			ItemType: domain.ImportItemType(it.ItemType), ExternalID: it.ExternalID, ItemTime: t,
		})
	}
	return queue
}

// fullDiscoveryPause is how long discovery waits out a rate limit (Strava's short window is 15 min).
var fullDiscoveryPause = 16 * time.Minute

// RunFullDiscovery walks every page of the account's activities, enqueuing
// each batch so the worker can start at once, and pauses across provider
// rate-limit windows until discovery is complete.
func (u *Imports) RunFullDiscovery(a domain.ExternalAccount, skipExisting bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	log := u.log().With("component", "full_discovery", "account_id", a.ID)

	_ = u.Accounts.UpdateAccountConfig(ctx, a.ID, map[string]any{"discovering": true})
	defer func() {
		_ = u.Accounts.UpdateAccountConfig(context.Background(), a.ID, map[string]any{"discovering": false})
	}()

	var existing map[string]struct{}
	if skipExisting {
		existing, _ = u.Sources.ExistingExternalIDs(ctx, a.Provider, a.ID)
	}

	page := 1
	discovered, enqueued := 0, 0
	for {
		b, err := u.discoverBatch(ctx, a, page)
		if err != nil {
			log.Warn("discover batch failed; stopping", "page", page, "error", err)
			break
		}
		discovered += len(b.Items)
		enqueued += u.enqueueDiscovered(ctx, log, buildImportQueue(b.Items, a, skipExisting, existing), !skipExisting)
		_ = u.Accounts.UpdateAccountConfig(ctx, a.ID, map[string]any{"last_discovered_total": discovered, "discovering": true})

		if b.Complete {
			_ = u.Accounts.UpdateAccountConfig(ctx, a.ID, map[string]any{"rate_limited_until": ""})
			break
		}
		page = b.NextPage
		if page <= 0 {
			break
		}
		if b.RateLimited && !u.waitRateLimit(ctx, log, a.ID, page) {
			return
		}
	}

	u.recordSyncStarted(ctx, a.ID, skipExisting, enqueued)
	log.Info("full discovery complete", "discovered", discovered, "enqueued", enqueued)
}

// enqueueDiscovered enqueues one batch and returns how many rows were new. A
// re-download (requeue) also resets rows an earlier sync finished.
func (u *Imports) enqueueDiscovered(ctx context.Context, log *slog.Logger, q []domain.ImportQueueItem, requeue bool) int {
	if len(q) == 0 {
		return 0
	}
	enqueue := u.Queue.Enqueue
	if requeue {
		enqueue = u.Queue.Requeue
	}
	n, err := enqueue(ctx, q)
	if err != nil {
		log.Warn("enqueue failed", "error", err)
		return 0
	}
	return n
}

// waitRateLimit marks the account rate-limited and waits out the window; false when ctx ended.
func (u *Imports) waitRateLimit(ctx context.Context, log *slog.Logger, id domain.ExternalAccountID, page int) bool {
	_ = u.Accounts.UpdateAccountConfig(ctx, id,
		map[string]any{"rate_limited_until": u.now().Add(RateLimitCooldown).Format(time.RFC3339)})
	log.Info("discovery paused on rate limit; resuming after window", "next_page", page)
	select {
	case <-time.After(fullDiscoveryPause):
		return true
	case <-ctx.Done():
		return false
	}
}

// recordSyncStarted records the history event when anything was enqueued.
func (u *Imports) recordSyncStarted(ctx context.Context, id domain.ExternalAccountID, skipExisting bool, enqueued int) {
	if u.Events == nil || enqueued <= 0 {
		return
	}
	detail := "Full sync"
	if skipExisting {
		detail = "Full sync (new only)"
	}
	_ = u.Events.Record(ctx, domain.ConnectionImportEvent{
		ExternalAccountID: id, Kind: domain.ImportEventSyncStarted, Count: enqueued, Detail: detail,
	})
}

// ImportOne queues one provider activity; false when it was already queued.
func (u *Imports) ImportOne(ctx context.Context, a domain.ExternalAccount, externalID string) (bool, error) {
	n, err := u.Queue.Enqueue(ctx, []domain.ImportQueueItem{{
		ExternalAccountID: a.ID, UserID: a.UserID, Provider: a.Provider,
		ItemType: domain.ImportItemActivity, ExternalID: externalID,
	}})
	if err != nil {
		return false, err
	}
	u.log().Info("single-activity import queued", "account_id", a.ID, "external_id", externalID, "new", n > 0)
	return n > 0, nil
}

// SetAutoImport suspends or resumes reconcile and webhook imports.
func (u *Imports) SetAutoImport(ctx context.Context, id domain.ExternalAccountID, enabled bool) error {
	return u.Accounts.SetAutoImport(ctx, id, enabled)
}

// SetPollInterval overrides the reconcile cadence; nil leaves it as is.
func (u *Imports) SetPollInterval(ctx context.Context, id domain.ExternalAccountID, seconds *int) error {
	patch := map[string]any{}
	if seconds != nil {
		patch["poll_interval_seconds"] = *seconds
	}
	return u.Accounts.UpdateAccountConfig(ctx, id, patch)
}

// QueueCounts are the account's queue items per status.
func (u *Imports) QueueCounts(ctx context.Context, id domain.ExternalAccountID) map[domain.ImportItemStatus]int {
	c, _ := u.Queue.CountByStatus(ctx, id)
	return c
}

// RequeueFailed retries one failed item; false when it isn't failed.
func (u *Imports) RequeueFailed(ctx context.Context, id domain.ExternalAccountID, item domain.ImportQueueItemID) (bool, error) {
	return u.Queue.RequeueFailed(ctx, id, item)
}

// MoveToTop claims one pending item next; false when it isn't pending.
func (u *Imports) MoveToTop(ctx context.Context, id domain.ExternalAccountID, item domain.ImportQueueItemID) (bool, error) {
	return u.Queue.MoveToTop(ctx, id, item)
}

// Reparse is the outcome of a bulk re-parse.
type Reparse struct{ Queued, Skipped, Remaining int }

// reparseBatch caps one bulk re-parse.
const reparseBatch = 500

// ReparseOutOfDate queues a quota-free re-parse for every source a newer
// parser of its pool can read, up to a batch.
func (u *Imports) ReparseOutOfDate(ctx context.Context, a domain.ExternalAccount) (Reparse, error) {
	srcs, err := u.Sources.ListSourcesOutOfDate(ctx, a.ID, "newer_parser", reparseBatch)
	if err != nil {
		return Reparse{}, err
	}
	var out Reparse
	for _, s := range srcs {
		if s.RawBlobID == "" {
			out.Skipped++
			continue
		}
		if _, err := u.Reimporter.Reparse(ctx, a.UserID, s); err != nil {
			u.log().Warn("bulk re-parse: dispatch failed", "source_id", s.ID, "error", err)
			out.Skipped++
			continue
		}
		out.Queued++
	}
	counts, _ := u.Sources.CountSourcesOutOfDate(ctx, a.ID)
	out.Remaining = counts["newer_parser"]
	return out, nil
}

// SetWorker pins the account to a known pool of its provider ("" unpins)
// under docs/adr/0006 and returns how many sources were flagged for a
// re-fetch by the new package.
func (u *Imports) SetWorker(ctx context.Context, a domain.ExternalAccount, target string, confirm bool) (int, error) {
	changesPackage, err := u.reassignCheck(ctx, a, target, confirm)
	if err != nil {
		return 0, err
	}
	if err := u.Accounts.SetAssignedWorker(ctx, a.ID, target); err != nil {
		if errors.Is(err, domain.ErrPoolTaken) {
			return 0, port.NewActionError(port.ActionErrorConflict, domain.ErrPoolTaken.Error())
		}
		return 0, port.NewActionError(port.ActionErrorInternal, "update failed")
	}
	if !changesPackage {
		return 0, nil
	}
	n, err := u.Sources.MarkSourcesPoolChanged(ctx, a.ID, target)
	if err != nil {
		u.log().Warn("flag sources after pool change", "account_id", a.ID, "error", err)
	}
	return n, nil
}

// reassignCheck: the target must be a known pool, a connection with a
// sibling on the same provider account cannot be unpinned, and moving onto
// another package needs the owner's confirmation.
func (u *Imports) reassignCheck(ctx context.Context, a domain.ExternalAccount, target string, confirm bool) (bool, error) {
	if target == "" {
		if u.hasSiblingConnection(ctx, a) {
			return false, port.NewActionError(port.ActionErrorConflict, "this provider account has another connection; connect it for the importer you want instead of unpinning")
		}
		return false, nil
	}
	if !slices.Contains(u.poolNames(ctx, a.Provider), target) {
		return false, port.NewActionError(port.ActionErrorInvalid, "no "+a.Provider+" importer named "+target)
	}
	pool, err := u.Pools.Get(ctx, a.Provider, target)
	if err != nil {
		return false, port.NewActionError(port.ActionErrorInvalid, "importer "+target+" has not registered yet")
	}
	workers, err := u.Sources.ListSourceWorkersForAccount(ctx, a.ID)
	if err != nil {
		return false, port.NewActionError(port.ActionErrorInternal, "could not inspect this connection's imports")
	}
	other, changes := otherPackageImporter(workers, target, pool.Package)
	if !changes {
		return false, nil
	}
	if !confirm {
		return false, port.NewActionError(port.ActionErrorConflict, "confirm: this connection's activities were imported by "+other.Name+" ("+other.Package+"). Moving to "+target+" keeps them, but they need a re-fetch by "+target+" to be updated by it. Continue?")
	}
	return true, nil
}

func (u *Imports) poolNames(ctx context.Context, provider string) []string {
	if u.Insight == nil {
		return nil
	}
	pools := u.Insight.Pools(ctx, provider)
	out := make([]string, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.Name)
	}
	return out
}

// hasSiblingConnection: the same provider account is connected again (on another pool).
func (u *Imports) hasSiblingConnection(ctx context.Context, a domain.ExternalAccount) bool {
	accts, err := u.Accounts.ListAccountsForUser(ctx, a.UserID)
	if err != nil {
		return false
	}
	for _, o := range accts {
		if o.ID != a.ID && o.Provider == a.Provider && o.ProviderAccountID != "" && o.ProviderAccountID == a.ProviderAccountID {
			return true
		}
	}
	return false
}

// otherPackageImporter returns the first importer whose package differs from the target pool's.
func otherPackageImporter(workers []domain.SourceWorker, target, pkg string) (domain.SourceWorker, bool) {
	for _, sw := range workers {
		if sw.Name != target && sw.Package != pkg {
			return sw, true
		}
	}
	return domain.SourceWorker{}, false
}
