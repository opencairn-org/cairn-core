package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ---------------------------------------------------------------------------
// Full-sync endpoints + the persisted import-queue processor.
//
// Flow: connect → POST .../sync/preview (discover + summarize, no enqueue) →
// user picks skip-vs-redownload → POST .../sync/start (discover + enqueue) →
// the processor goroutine drains the queue paced + rate-limit-aware →
// result_router marks each item done after ingest.
// ---------------------------------------------------------------------------

type syncDiscoverItem struct {
	ItemType   string `json:"item_type"`
	ExternalID string `json:"external_id"`
	ItemTime   string `json:"item_time"`
}

type syncDiscoverResp struct {
	Items       []syncDiscoverItem `json:"items"`
	Complete    bool               `json:"complete"`
	NextPage    int                `json:"next_page,omitempty"`
	RateLimited bool               `json:"rate_limited,omitempty"`
	Error       string             `json:"error,omitempty"`
}

// fetchSourceJobBody mirrors the worker's fetch_source job input (JSON job
// bodies — distinct from the typed proto JobResult the worker returns).
type fetchSourceJobBody struct {
	JobID        string `json:"job_id"`
	AccountID    string `json:"account_id"`
	UserID       string `json:"user_id"`
	Provider     string `json:"provider"`
	ExtID        string `json:"ext_id"`
	FetchStreams bool   `json:"fetch_streams"`
	Reason       string `json:"reason"`
}

func mountSyncEndpoints(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.NATSBus == nil {
		logger.Info("sync endpoints not mounted: NATS not wired")
		return
	}
	h := syncHandlers{app: app, logger: logger}

	// GET /api/accounts — the user's connected accounts + per-account queue.
	mux.HandleFunc("GET /api/accounts", h.listAccounts)

	// PUT /api/accounts/{id}/auto-import {enabled} — suspend/resume automatic
	// imports (reconcile + webhook fetch) for this account (#93).
	mux.HandleFunc("PUT /api/accounts/{id}/auto-import", h.setAutoImport)

	// PUT /api/accounts/{id}/worker {worker} — route this account's jobs to a
	// named worker pool (docs/adr/0006).
	mux.HandleFunc("PUT /api/accounts/{id}/worker", h.setWorker)

	// POST /api/accounts/{id}/reparse-out-of-date — bulk re-parse (docs/adr/0005).
	mux.HandleFunc("POST /api/accounts/{id}/reparse-out-of-date", h.reparseOutOfDate)

	// POST /api/accounts/{id}/sync/preview — discover + summarize (no enqueue).
	mux.HandleFunc("POST /api/accounts/{id}/sync/preview", h.syncPreview)

	// POST /api/accounts/{id}/sync/start {skip_existing} — discover + enqueue.
	mux.HandleFunc("POST /api/accounts/{id}/sync/start", h.syncStart)

	// POST /api/accounts/{id}/import-one {external_id} — explicitly import (or
	// re-import) a single activity by its provider ID. Provider-generic: it
	// rides the same persisted queue → processor → fetch_source job path as
	// full sync, so pacing, rate limits, and result routing all apply.
	mux.HandleFunc("POST /api/accounts/{id}/import-one", h.importOne)

	// PUT /api/accounts/{id}/config — per-connection settings (poll interval).
	mux.HandleFunc("PUT /api/accounts/{id}/config", h.putConfig)

	// GET /api/accounts/{id}/history — the connection's import history.
	mux.HandleFunc("GET /api/accounts/{id}/history", h.history)

	// GET /api/accounts/{id}/queue — queue status.
	mux.HandleFunc("GET /api/accounts/{id}/queue", h.queue)

	// GET /api/accounts/{id}/queue/items?status=pending,in_progress&limit=100 —
	// the actual queue rows, for debugging a stuck import from the UI. Default
	// filter is the "live" set (everything not done); pass status=all for the
	// full picture including done items.
	mux.HandleFunc("GET /api/accounts/{id}/queue/items", h.queueItems)

	// POST /api/accounts/{id}/queue/items/{itemID}/requeue — flip one failed
	// item back to pending (attempts reset) so the processor retries it.
	mux.HandleFunc("POST /api/accounts/{id}/queue/items/{itemID}/requeue", h.queueItemAction(
		func(ctx context.Context, id domain.ExternalAccountID, itemID domain.ImportQueueItemID) (bool, error) {
			return app.ImportQueue.RequeueFailed(ctx, id, itemID)
		}, "item is not in failed state", "requeued"))

	// POST /api/accounts/{id}/queue/items/{itemID}/move-to-top — bump one
	// pending item above the rest so it is claimed on the next tick.
	mux.HandleFunc("POST /api/accounts/{id}/queue/items/{itemID}/move-to-top", h.queueItemAction(
		func(ctx context.Context, id domain.ExternalAccountID, itemID domain.ImportQueueItemID) (bool, error) {
			return app.ImportQueue.MoveToTop(ctx, id, itemID)
		}, "item is not in pending state", "moved"))

	logger.Info("sync endpoints mounted")
}

// discoverItems asks the provider's worker (request/reply) what's importable.
// discoverBatch asks the worker for one batch of activities starting at
// startPage. The worker walks up to discoverMaxPagesPerCall pages and returns
// Complete (reached the 0-item end page), or NextPage + RateLimited so the
// caller can resume.
func discoverBatch(ctx context.Context, app *App, a domain.ExternalAccount, startPage int) (syncDiscoverResp, error) {
	reqBody, _ := json.Marshal(map[string]any{"account_id": a.ID.String(), "start_page": startPage})
	name, err := (presenceWorkerRouter{app: app, logger: slog.Default()}).AccountWorker(ctx, a)
	if err != nil {
		return syncDiscoverResp{}, err
	}
	respBytes, err := app.NATSBus.Request(ctx, domain.DiscoverSubject(a.Provider, name), reqBody, 90*time.Second)
	if err != nil {
		return syncDiscoverResp{}, err
	}
	var resp syncDiscoverResp
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return syncDiscoverResp{}, err
	}
	if resp.Error != "" {
		return syncDiscoverResp{}, fmt.Errorf("worker: %s", resp.Error)
	}
	return resp, nil
}

// discoverAllNoWait walks every page of activities WITHOUT waiting on rate
// limits — for the preview estimate. Returns the items found so far and whether
// discovery reached the true end (complete). When a rate limit is hit mid-walk
// it returns the partial set with complete=false (the count is a lower bound).
func discoverAllNoWait(ctx context.Context, app *App, a domain.ExternalAccount) (items []syncDiscoverItem, complete bool, err error) {
	page := 1
	for iter := 0; iter < 2000; iter++ { // safety bound on round-trips
		b, e := discoverBatch(ctx, app, a, page)
		if e != nil {
			return items, false, e
		}
		items = append(items, b.Items...)
		if b.Complete {
			return items, true, nil
		}
		if b.RateLimited {
			return items, false, nil // partial; preview doesn't block on refills
		}
		page = b.NextPage
		if page <= 0 {
			return items, true, nil
		}
	}
	return items, false, nil
}

// buildImportQueue turns discovered items into queue rows, optionally skipping
// already-imported external ids.
func buildImportQueue(items []syncDiscoverItem, a domain.ExternalAccount, skipExisting bool, existing map[string]struct{}) []domain.ImportQueueItem {
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

// runFullDiscovery is the background worker behind Full Sync: it walks EVERY
// page of the account's activities (to the 0-item end), enqueuing each batch
// incrementally so the worker can start pulling immediately, and PAUSES then
// RESUMES across provider rate-limit windows until discovery is complete. Runs
// detached from the request (the HTTP call returns "started").
func runFullDiscovery(app *App, logger *slog.Logger, a domain.ExternalAccount, skipExisting bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	log := logger.With("component", "full_discovery", "account_id", a.ID)

	_ = app.ExternalAccounts.UpdateAccountConfig(ctx, a.ID, map[string]any{"discovering": true})
	defer func() {
		_ = app.ExternalAccounts.UpdateAccountConfig(context.Background(), a.ID, map[string]any{"discovering": false})
	}()

	var existing map[string]struct{}
	if skipExisting {
		existing, _ = app.Activities.ExistingExternalIDs(ctx, a.Provider, a.ID)
	}

	page := 1
	discovered, enqueued := 0, 0
	for {
		b, err := discoverBatch(ctx, app, a, page)
		if err != nil {
			log.Warn("discover batch failed; stopping", "page", page, "error", err)
			break
		}
		discovered += len(b.Items)
		enqueued += fullDiscoveryEnqueue(ctx, log, app, buildImportQueue(b.Items, a, skipExisting, existing))
		_ = app.ExternalAccounts.UpdateAccountConfig(ctx, a.ID,
			map[string]any{"last_discovered_total": discovered, "discovering": true})

		if b.Complete {
			_ = app.ExternalAccounts.UpdateAccountConfig(ctx, a.ID, map[string]any{"rate_limited_until": ""})
			break
		}
		page = b.NextPage
		if page <= 0 {
			break
		}
		if b.RateLimited && !fullDiscoveryWaitRateLimit(ctx, log, app, a.ID, page) {
			return
		}
	}

	fullDiscoveryRecordEvent(ctx, app, a.ID, skipExisting, enqueued)
	log.Info("full discovery complete", "discovered", discovered, "enqueued", enqueued)
}

// fullDiscoveryEnqueue enqueues one discovered batch and returns how many rows were new.
func fullDiscoveryEnqueue(ctx context.Context, log *slog.Logger, app *App, q []domain.ImportQueueItem) int {
	if len(q) == 0 {
		return 0
	}
	n, e := app.ImportQueue.Enqueue(ctx, q)
	if e != nil {
		log.Warn("enqueue failed", "error", e)
		return 0
	}
	return n
}

// fullDiscoveryWaitRateLimit marks the account rate-limited and waits out the window; false when ctx ended.
func fullDiscoveryWaitRateLimit(ctx context.Context, log *slog.Logger, app *App, id domain.ExternalAccountID, page int) bool {
	_ = app.ExternalAccounts.UpdateAccountConfig(ctx, id,
		map[string]any{"rate_limited_until": time.Now().UTC().Add(rateLimitCooldown).Format(time.RFC3339)})
	log.Info("discovery paused on rate limit; resuming after window", "next_page", page)
	select {
	case <-time.After(16 * time.Minute): // Strava short window is 15 min
		return true
	case <-ctx.Done():
		return false
	}
}

// fullDiscoveryRecordEvent records the sync-started history event when anything was enqueued.
func fullDiscoveryRecordEvent(ctx context.Context, app *App, id domain.ExternalAccountID, skipExisting bool, enqueued int) {
	if app.ImportEvents == nil || enqueued <= 0 {
		return
	}
	detail := "Full sync"
	if skipExisting {
		detail = "Full sync (new only)"
	}
	_ = app.ImportEvents.Record(ctx, domain.ConnectionImportEvent{
		ExternalAccountID: id, Kind: domain.ImportEventSyncStarted, Count: enqueued, Detail: detail,
	})
}

// ---------------------------------------------------------------------------
// Processor goroutine
// ---------------------------------------------------------------------------

const (
	queueMaxInFlightPerAccount = 5
	queueProcessorTick         = 8 * time.Second

	// Items dispatched but without a result for this long are considered
	// stale: the fetch job was Term'd on the worker (DLQ) or its result got
	// lost, and nothing else will ever complete them. They're requeued (or
	// failed at queueStaleMaxAttempts) so they can't pin a queue slot forever.
	queueStaleAfter       = 30 * time.Minute
	queueStaleMaxAttempts = 5
	queueStaleReapEvery   = 5 * time.Minute
)

// runImportQueueProcessor drains the persisted import queue, paced and
// rate-limit-aware. Each tick it scans accounts with pending items and, for
// each that isn't rate-limited and has capacity, claims a batch and dispatches
// fetch jobs. Items stay in_progress until the result router marks them done.
func runImportQueueProcessor(ctx context.Context, logger *slog.Logger, app *App) {
	log := logger.With("component", "import_queue_processor")
	log.Info("import queue processor started", "tick", queueProcessorTick)
	ticker := time.NewTicker(queueProcessorTick)
	defer ticker.Stop()
	reap := time.NewTicker(queueStaleReapEvery)
	defer reap.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("import queue processor shutting down")
			return
		case <-reap.C:
			requeued, failed, err := app.ImportQueue.RequeueStale(ctx, queueStaleAfter, queueStaleMaxAttempts)
			if err != nil {
				log.Warn("stale queue reap failed", "error", err)
			} else if requeued > 0 || failed > 0 {
				log.Info("stale queue items reaped", "requeued", requeued, "failed", failed)
			}
		case <-ticker.C:
			accts, err := app.ImportQueue.AccountsWithPending(ctx, 100)
			if err != nil {
				log.Warn("scan pending accounts failed", "error", err)
				continue
			}
			for _, acctID := range accts {
				processAccountQueue(ctx, log, app, acctID)
			}
		}
	}
}

func processAccountQueue(ctx context.Context, log *slog.Logger, app *App, acctID domain.ExternalAccountID) {
	acct, err := app.ExternalAccounts.GetExternalAccount(ctx, acctID)
	if err != nil {
		log.Warn("queue: load account failed", "account_id", acctID, "error", err)
		return
	}
	if rateLimitedNow(acct.RateLimit) {
		return // paused until the window resets
	}

	counts, err := app.ImportQueue.CountByStatus(ctx, acctID)
	if err != nil {
		log.Warn("queue: count by status failed", "account_id", acctID, "error", err)
		return
	}
	avail := queueMaxInFlightPerAccount - counts[domain.ImportStatusInProgress]
	if avail <= 0 {
		return
	}

	subject, err := accountJobSubject(ctx, app, log, "fetch_source", acct)
	if err != nil {
		log.Warn("queue: no worker pool for account", "account_id", acctID, "error", err)
		return
	}
	items, err := app.ImportQueue.ClaimPending(ctx, acctID, avail)
	if err != nil {
		log.Warn("queue: claim pending failed", "account_id", acctID, "error", err)
		return
	}
	if len(items) == 0 {
		return
	}
	for _, it := range items {
		body, _ := json.Marshal(fetchSourceJobBody{
			JobID:        it.ID.String(),
			AccountID:    acctID.String(),
			UserID:       it.UserID.String(),
			Provider:     it.Provider,
			ExtID:        it.ExternalID,
			FetchStreams: true,
			Reason:       "queue",
		})
		msgID := "queue:" + it.Provider + ":" + acctID.String() + ":" + it.ExternalID
		if err := app.NATSBus.Publish(ctx, subject, msgID, body); err != nil {
			_ = app.ImportQueue.MarkFailed(ctx, it.ID, "dispatch: "+err.Error())
		}
		// Item stays in_progress; result_router marks it done after ingest.
	}
	log.Info("dispatched queue items", "account_id", acctID, "n", len(items))
}

// rateLimitCooldown is how long a connection is marked rate-limited in the UI
// after the worker reports an exhausted provider budget. The worker doesn't
// return a precise reset time, so we use a conservative cooldown; a successful
// discover clears it early.
const rateLimitCooldown = 30 * time.Minute

// isRateLimitErr reports whether a discover error is the worker's
// provider-budget-exhausted signal (vs a real failure).
func isRateLimitErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "rate_limited") || strings.Contains(s, "budget exhausted") ||
		strings.Contains(s, "rate limited")
}

// writeRateLimited, when err is a rate-limit signal, persists a cooldown marker
// on the account (so /api/accounts reports rate_limited=true and the status
// badge shows it) and responds 429 with a clear message. Returns true when it
// handled the error.
func writeRateLimited(w http.ResponseWriter, app *App, r *http.Request, id domain.ExternalAccountID, err error) bool {
	if !isRateLimitErr(err) {
		return false
	}
	until := time.Now().UTC().Add(rateLimitCooldown)
	_ = app.ExternalAccounts.UpdateAccountConfig(r.Context(), id,
		map[string]any{"rate_limited_until": until.Format(time.RFC3339)})
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":   "rate_limited",
		"message": "Provider daily/short budget exhausted — no new activities can be fetched right now. Try again later.",
		"until":   until.Format(time.RFC3339),
	})
	return true
}

// rateLimitKVState mirrors the worker rate limiter's KV value (the provider
// repos' internal/nats rateLimitState). The worker syncs it from the
// provider's usage headers, so it's the live view of the real API budget.
type rateLimitKVState struct {
	Capacity      int       `json:"capacity"`
	Used          int       `json:"used"`
	WindowResetAt time.Time `json:"window_reset_at"`
}

// kvBus is the slice of the NATS bus providerBudget needs — taking the
// interface (not *App) lets wire.go build the reconcile budget gate before
// the App struct exists.
type kvBus interface {
	KV(bucket string) (port.KV, error)
}

// providerBudget reads the pool's live API budget from the shared
// cairn_rate_limits KV (keys <provider>.<pool>.short / .daily, docs/adr/0006;
// a 0.4 worker writes <provider>.short / .daily, read as a fallback). Returns a
// UI-friendly map per window plus whether any window is currently exhausted —
// this is what flips the connection badge to "Rate limited" while the worker
// waits out a 429, and what the reconcile scheduler's budget gate keys on.
// Missing bucket/keys mean "no worker has reported yet".
func providerBudget(ctx context.Context, bus kvBus, provider, pool string) (map[string]any, bool) {
	if bus == nil {
		return nil, false
	}
	kv, err := bus.KV("cairn_rate_limits")
	if err != nil {
		return nil, false
	}
	out := map[string]any{}
	limited := false
	now := time.Now().UTC()
	for _, win := range []string{"short", "daily"} {
		entry, err := kv.Get(ctx, provider+"."+pool+"."+win)
		if pool == "" || err != nil {
			if entry, err = kv.Get(ctx, provider+"."+win); err != nil {
				continue
			}
		}
		var st rateLimitKVState
		if json.Unmarshal(entry.Value, &st) != nil || st.Capacity <= 0 {
			continue
		}
		active := now.Before(st.WindowResetAt)
		used := st.Used
		if !active {
			used = 0 // window elapsed; budget is fresh even if no call re-synced yet
		}
		out[win] = map[string]any{
			"used":      used,
			"limit":     st.Capacity,
			"resets_at": st.WindowResetAt.UTC().Format(time.RFC3339),
		}
		if active && used >= st.Capacity {
			limited = true
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, limited
}

// rateLimitedNow reports whether either provider window is exhausted and not
// yet reset.
func rateLimitedNow(rl *domain.RateLimitSnapshot) bool {
	if rl == nil {
		return false
	}
	now := time.Now()
	if rl.ShortWindowLimit > 0 && rl.ShortWindowUsed >= rl.ShortWindowLimit && now.Before(rl.ShortWindowResetAt) {
		return true
	}
	if rl.LongWindowLimit > 0 && rl.LongWindowUsed >= rl.LongWindowLimit && now.Before(rl.LongWindowResetAt) {
		return true
	}
	return false
}
