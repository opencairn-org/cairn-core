package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/postgres"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// syncHandlers holds the dependencies of the account/sync HTTP handlers.
type syncHandlers struct {
	app    *App
	logger *slog.Logger
}

func (h syncHandlers) queueCounts(ctx context.Context, id domain.ExternalAccountID) map[string]int {
	c, _ := h.app.ImportQueue.CountByStatus(ctx, id)
	return map[string]int{
		"pending":     c[domain.ImportStatusPending],
		"in_progress": c[domain.ImportStatusInProgress],
		"done":        c[domain.ImportStatusDone],
		"failed":      c[domain.ImportStatusFailed],
		"skipped":     c[domain.ImportStatusSkipped],
	}
}

func (h syncHandlers) ownAccount(r *http.Request) (domain.ExternalAccount, bool) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		return domain.ExternalAccount{}, false
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return domain.ExternalAccount{}, false
	}
	a, err := h.app.ExternalAccounts.GetExternalAccount(r.Context(), domain.ExternalAccountID(id))
	if err != nil || a.UserID != userID {
		return domain.ExternalAccount{}, false
	}
	return a, true
}

// listAccounts serves GET /api/accounts.
func (h syncHandlers) listAccounts(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	accts, err := h.app.ExternalAccounts.ListAccountsForUser(r.Context(), userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(accts))
	// Live API budget per provider (memoised — accounts often share one).
	// Budgets are per provider app (the connection's client id).
	budgets := map[string]map[string]any{}
	budgetLimited := map[string]bool{}
	appOf := map[domain.ExternalAccountID]string{}
	for _, a := range accts {
		appOf[a.ID] = accountAppID(r.Context(), h.app, a)
		k := a.Provider + "/" + appOf[a.ID] + "/" + a.AssignedWorkerName
		if _, seen := budgets[k]; !seen {
			budgets[k], budgetLimited[k] = providerBudget(r.Context(), h.app.NATSBus, a.Provider, appOf[a.ID], a.AssignedWorkerName)
		}
	}
	for _, a := range accts {
		k := a.Provider + "/" + appOf[a.ID] + "/" + a.AssignedWorkerName
		row := h.accountRow(r.Context(), a, budgets[k], budgetLimited[k])
		// The pool this account's jobs go to, and the pools it could move to
		// (docs/adr/0006).
		row["assigned_worker"] = a.AssignedWorkerName
		row["worker_pools"] = workerPoolsJSON(r.Context(), h.app, h.logger, a.Provider)
		if n, err := h.app.Activities.CountSourcesOutOfDate(r.Context(), a.ID); err == nil {
			row["out_of_date"] = n
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": out,
		// Instance-wide reconcile cadence — shown next to the per-connection
		// override so "0 = instance default" names a concrete number.
		"default_poll_interval_seconds": int(h.app.DefaultPollInterval.Seconds()),
	})
}

// syncAccountConfigView is the per-connection config surfaced by GET /api/accounts.
type syncAccountConfigView struct {
	pollInterval  int
	providerTotal int
	discovering   bool
	rateLimited   bool
}

// syncParseAccountConfig reads the UI-relevant keys of an account's config blob.
func syncParseAccountConfig(raw []byte) syncAccountConfigView {
	var v syncAccountConfigView
	if len(raw) == 0 {
		return v
	}
	var cfg map[string]any
	if json.Unmarshal(raw, &cfg) != nil {
		return v
	}
	if f, ok := cfg["poll_interval_seconds"].(float64); ok {
		v.pollInterval = int(f)
	}
	if f, ok := cfg["last_discovered_total"].(float64); ok {
		v.providerTotal = int(f)
	}
	if b, ok := cfg["discovering"].(bool); ok {
		v.discovering = b
	}
	// Worker-reported budget exhaustion (set by sync/preview|start
	// on a rate-limit error) — surfaced until the cooldown lapses.
	if s, ok := cfg["rate_limited_until"].(string); ok && s != "" {
		if until, err := time.Parse(time.RFC3339, s); err == nil && time.Now().Before(until) {
			v.rateLimited = true
		}
	}
	return v
}

// syncFormatTimePtr formats t as RFC3339 UTC, or nil when unset.
func syncFormatTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func (h syncHandlers) accountRow(ctx context.Context, a domain.ExternalAccount, budget map[string]any, budgetLimited bool) map[string]any {
	// Total activities already imported from this account (count of
	// its sources). Cheap for single-user instances.
	imported := 0
	if existing, e := h.app.Activities.ExistingExternalIDs(ctx, a.Provider, a.ID); e == nil {
		imported = len(existing)
	}
	// Per-connection config (currently the poll interval). 0 = use the
	// instance default.
	cfg := syncParseAccountConfig(a.Config)
	rateLimited := rateLimitedNow(a.RateLimit) || budgetLimited || cfg.rateLimited
	providerTotal := cfg.providerTotal
	// last_discovered_total is a snapshot from the last user-driven
	// discovery; reconcile imports activities without refreshing it,
	// so the live imported count can overtake it ("906 / 905"). An
	// imported external id is proof the provider had at least that
	// many — clamp instead of showing an impossible ratio. Zero stays
	// zero: it means "never discovered" and the UI hides the total.
	if providerTotal > 0 && imported > providerTotal {
		providerTotal = imported
	}
	var connectionID any
	if a.ConnectionID != nil {
		connectionID = a.ConnectionID.String()
	}
	return map[string]any{
		"id": a.ID.String(), "provider": a.Provider,
		"connection_id":       connectionID,
		"provider_account_id": a.ProviderAccountID,
		"label":               a.DisplayLabel, "status": string(a.Status),
		"imported":              imported,
		"provider_total":        providerTotal,
		"last_sync_at":          syncFormatTimePtr(a.LastSyncAt),
		"watermark":             syncFormatTimePtr(a.SyncWatermark),
		"rate_limited":          rateLimited,
		"rate_limit_budget":     budget,
		"discovering":           cfg.discovering,
		"poll_interval_seconds": cfg.pollInterval,
		"auto_import_enabled":   a.AutoImportEnabled,
		"queue":                 h.queueCounts(ctx, a.ID),
	}
}

// setWorker serves PUT /api/accounts/{id}/worker {worker, confirm}: pins the
// account to a known pool of its provider; "" unpins (the only pool is used).
func (h syncHandlers) setWorker(w http.ResponseWriter, r *http.Request) {
	a, ok := h.ownAccount(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var body struct {
		Worker string `json:"worker"`
		// Confirm accepts moving to another implementation: the connection's
		// activities are flagged for a re-fetch by the new pool.
		Confirm bool `json:"confirm"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	status, msg, changesPackage := h.reassignCheck(r.Context(), a, body.Worker, body.Confirm)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	if err := h.app.ExternalAccounts.SetAssignedWorker(r.Context(), a.ID, body.Worker); err != nil {
		if errors.Is(err, domain.ErrPoolTaken) {
			http.Error(w, domain.ErrPoolTaken.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	flagged := 0
	if changesPackage {
		n, err := h.app.Activities.MarkSourcesPoolChanged(r.Context(), a.ID, body.Worker)
		if err != nil {
			h.logger.Warn("flag sources after pool change", "account_id", a.ID, "error", err)
		}
		flagged = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"assigned_worker": body.Worker, "pool_changed": flagged})
}

// reassignCheck applies the rules for moving a connection to another pool
// (docs/adr/0006): the target must be a known pool of the same package as
// the connection's imports (a different implementation is a separate
// connection), and a connection cannot be unpinned while the same provider
// account has another connection.
// Onto another implementation the owner must confirm; the connection's
// activities are then flagged pool_changed for a re-fetch by the new pool.
func (h syncHandlers) reassignCheck(ctx context.Context, a domain.ExternalAccount, target string, confirm bool) (status int, msg string, changesPackage bool) {
	if target == "" {
		if h.hasSiblingConnection(ctx, a) {
			return http.StatusConflict, "this provider account has another connection; connect it for the importer you want instead of unpinning", false
		}
		return 0, "", false
	}
	if !slices.Contains(knownPoolNames(ctx, h.app, h.logger, a.Provider), target) {
		return http.StatusBadRequest, "no " + a.Provider + " importer named " + target, false
	}
	pool, err := h.app.WorkerPools.Get(ctx, a.Provider, target)
	if err != nil {
		return http.StatusBadRequest, "importer " + target + " has not registered yet", false
	}
	workers, err := h.app.Activities.ListSourceWorkersForAccount(ctx, a.ID)
	if err != nil {
		return http.StatusInternalServerError, "could not inspect this connection's imports", false
	}
	other, changes := otherPackageImporter(workers, target, pool.Package)
	if !changes {
		return 0, "", false
	}
	if !confirm {
		return http.StatusConflict, "confirm: this connection's activities were imported by " + other.Name + " (" + other.Package + "). Moving to " + target + " keeps them, but they need a re-fetch by " + target + " to be updated by it. Continue?", false
	}
	return 0, "", true
}

// hasSiblingConnection: the same provider account is connected again (on
// another pool).
func (h syncHandlers) hasSiblingConnection(ctx context.Context, a domain.ExternalAccount) bool {
	accts, err := h.app.ExternalAccounts.ListAccountsForUser(ctx, a.UserID)
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

// otherPackageImporter returns the first importer of the connection's sources
// whose package differs from the target pool's.
func otherPackageImporter(workers []postgres.SourceWorker, target, pkg string) (postgres.SourceWorker, bool) {
	for _, sw := range workers {
		if sw.Name != target && sw.Package != pkg {
			return sw, true
		}
	}
	return postgres.SourceWorker{}, false
}

// reparseOutOfDate serves POST /api/accounts/{id}/reparse-out-of-date: queues
// a quota-free re-parse for every source a newer parser of its pool can
// read (reason newer_parser), up to a batch; the reply says how many remain.
func (h syncHandlers) reparseOutOfDate(w http.ResponseWriter, r *http.Request) {
	a, ok := h.ownAccount(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	const batch = 500
	srcs, err := h.app.Activities.ListSourcesOutOfDate(r.Context(), a.ID, "newer_parser", batch)
	if err != nil {
		http.Error(w, "could not list sources", http.StatusInternalServerError)
		return
	}
	queued, skipped := 0, 0
	for _, s := range srcs {
		if s.RawBlobID == "" {
			skipped++
			continue
		}
		if _, err := newSourceReimporter(h.app, h.logger).Reparse(r.Context(), a.UserID, s); err != nil {
			h.logger.Warn("bulk re-parse: dispatch failed", "source_id", s.ID, "error", err)
			skipped++
			continue
		}
		queued++
	}
	counts, _ := h.app.Activities.CountSourcesOutOfDate(r.Context(), a.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": queued, "skipped": skipped, "remaining": counts["newer_parser"]})
}

// setAutoImport serves PUT /api/accounts/{id}/auto-import.
func (h syncHandlers) setAutoImport(w http.ResponseWriter, r *http.Request) {
	a, ok := h.ownAccount(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.app.ExternalAccounts.SetAutoImport(r.Context(), a.ID, body.Enabled); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auto_import_enabled": body.Enabled})
}

// syncPreview serves POST /api/accounts/{id}/sync/preview.
func (h syncHandlers) syncPreview(w http.ResponseWriter, r *http.Request) {
	a, ok := h.ownAccount(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	items, complete, err := discoverAllNoWait(r.Context(), h.app, a)
	if err != nil {
		if writeRateLimited(w, h.app, r, a.ID, err) {
			return
		}
		http.Error(w, "discover failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Persist the provider-side total so the connection card can show
	// "imported X / Y" even without re-running discovery. A successful
	// discover also clears any stale rate-limit marker.
	_ = h.app.ExternalAccounts.UpdateAccountConfig(r.Context(), a.ID,
		map[string]any{"last_discovered_total": len(items), "rate_limited_until": ""})
	existing, _ := h.app.Activities.ExistingExternalIDs(r.Context(), a.Provider, a.ID)
	present := 0
	for _, it := range items {
		if _, ok := existing[it.ExternalID]; ok {
			present++
		}
	}
	// complete=false means a rate limit cut the preview walk short — the
	// counts are a lower bound; the full sync will discover the rest.
	writeJSON(w, http.StatusOK, map[string]any{
		"total": len(items), "already_present": present, "new": len(items) - present,
		"complete": complete,
	})
}

// syncStart serves POST /api/accounts/{id}/sync/start.
func (h syncHandlers) syncStart(w http.ResponseWriter, r *http.Request) {
	a, ok := h.ownAccount(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		SkipExisting bool `json:"skip_existing"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	// Full Sync runs in the background: it walks EVERY page (to the 0-item
	// end), enqueues incrementally, and resumes across rate-limit windows.
	// Returning immediately keeps the request fast; the UI tracks progress
	// via the queue counts + the "discovering" flag.
	go runFullDiscovery(h.app, h.logger, a, body.SkipExisting)
	h.logger.Info("full discovery started", "account_id", a.ID, "skip_existing", body.SkipExisting)
	writeJSON(w, http.StatusOK, map[string]any{"started": true})
}

// importOne serves POST /api/accounts/{id}/import-one.
func (h syncHandlers) importOne(w http.ResponseWriter, r *http.Request) {
	a, ok := h.ownAccount(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	body.ExternalID = strings.TrimSpace(body.ExternalID)
	if body.ExternalID == "" {
		http.Error(w, "external_id required", http.StatusBadRequest)
		return
	}
	n, err := h.app.ImportQueue.Enqueue(r.Context(), []domain.ImportQueueItem{{
		ExternalAccountID: a.ID, UserID: a.UserID, Provider: a.Provider,
		ItemType: domain.ImportItemActivity, ExternalID: body.ExternalID,
	}})
	if err != nil {
		http.Error(w, "enqueue failed", http.StatusInternalServerError)
		return
	}
	h.logger.Info("single-activity import queued", "account_id", a.ID, "external_id", body.ExternalID, "new", n > 0)
	writeJSON(w, http.StatusOK, map[string]any{"queued": n > 0, "already_queued": n == 0})
}

// putConfig serves PUT /api/accounts/{id}/config.
func (h syncHandlers) putConfig(w http.ResponseWriter, r *http.Request) {
	a, ok := h.ownAccount(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		PollIntervalSeconds *int `json:"poll_interval_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	patch := map[string]any{}
	if body.PollIntervalSeconds != nil {
		v := *body.PollIntervalSeconds
		if v < 0 {
			http.Error(w, "poll_interval_seconds must be >= 0", http.StatusBadRequest)
			return
		}
		patch["poll_interval_seconds"] = v
	}
	if err := h.app.ExternalAccounts.UpdateAccountConfig(r.Context(), a.ID, patch); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// history serves GET /api/accounts/{id}/history.
func (h syncHandlers) history(w http.ResponseWriter, r *http.Request) {
	a, ok := h.ownAccount(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	events := []map[string]any{}
	if h.app.ImportEvents != nil {
		list, _ := h.app.ImportEvents.ListForAccount(r.Context(), a.ID, 50)
		for _, e := range list {
			events = append(events, h.historyEvent(r.Context(), a, e))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (h syncHandlers) historyEvent(ctx context.Context, a domain.ExternalAccount, e domain.ConnectionImportEvent) map[string]any {
	ev := map[string]any{
		"kind":        e.Kind,
		"count":       e.Count,
		"detail":      e.Detail,
		"external_id": e.ExternalID,
		"occurred_at": e.OccurredAt.UTC().Format(time.RFC3339),
	}
	// Resolve the internal activity id for per-activity events so the
	// UI can deep-link to the imported activity, plus an external deep
	// link to the original on the provider's site.
	linked := false
	if e.ExternalID != "" && (e.Kind == domain.ImportEventActivityImported || e.Kind == domain.ImportEventActivityUpdated) {
		if src, err := h.app.Activities.FindSourceByExternalID(ctx, a.Provider, &a.ID, e.ExternalID); err == nil {
			ev["activity_id"] = src.ActivityID.String()
			linked = true
		}
		if ext := providerActivityURL(a.Provider, e.ExternalID); ext != "" {
			ev["external_url"] = ext
		}
	}
	// Per-entry status: failed; completed (the activity is in and
	// linkable); or ongoing (queued/processing — no link yet).
	switch {
	case e.Kind == domain.ImportEventFailed:
		ev["status"] = "failed"
	case linked:
		ev["status"] = "completed"
	default:
		ev["status"] = "ongoing"
	}
	return ev
}

// queue serves GET /api/accounts/{id}/queue.
func (h syncHandlers) queue(w http.ResponseWriter, r *http.Request) {
	a, ok := h.ownAccount(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, h.queueCounts(r.Context(), a.ID))
}

// syncQueueItemsStatuses parses the ?status= filter (default: everything not done).
func syncQueueItemsStatuses(q string) []domain.ImportItemStatus {
	statuses := []domain.ImportItemStatus{domain.ImportStatusPending, domain.ImportStatusInProgress, domain.ImportStatusFailed}
	if q == "" {
		return statuses
	}
	if q == "all" {
		return nil
	}
	statuses = statuses[:0]
	for s := range strings.SplitSeq(q, ",") {
		if s = strings.TrimSpace(s); s != "" {
			statuses = append(statuses, domain.ImportItemStatus(s))
		}
	}
	return statuses
}

// queueItems serves GET /api/accounts/{id}/queue/items.
func (h syncHandlers) queueItems(w http.ResponseWriter, r *http.Request) {
	a, ok := h.ownAccount(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	statuses := syncQueueItemsStatuses(r.URL.Query().Get("status"))
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	items, err := h.app.ImportQueue.ListForAccount(r.Context(), a.ID, statuses, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, syncQueueItemRow(a.Provider, it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func syncQueueItemRow(provider string, it domain.ImportQueueItem) map[string]any {
	row := map[string]any{
		"id":          it.ID.String(),
		"item_type":   string(it.ItemType),
		"external_id": it.ExternalID,
		"status":      string(it.Status),
		"priority":    it.Priority,
		"attempts":    it.Attempts,
		"last_error":  it.LastError,
		"created_at":  it.CreatedAt.UTC().Format(time.RFC3339),
	}
	if it.ItemTime != nil {
		row["item_time"] = it.ItemTime.UTC().Format(time.RFC3339)
	}
	if it.StartedAt != nil {
		row["started_at"] = it.StartedAt.UTC().Format(time.RFC3339)
	}
	if it.CompletedAt != nil {
		row["completed_at"] = it.CompletedAt.UTC().Format(time.RFC3339)
	}
	if url := providerActivityURL(provider, it.ExternalID); url != "" {
		row["external_url"] = url
	}
	return row
}

// queueItemAction runs a single-item queue mutation and replies {respKey: true}.
func (h syncHandlers) queueItemAction(
	op func(ctx context.Context, id domain.ExternalAccountID, itemID domain.ImportQueueItemID) (bool, error),
	conflictMsg, respKey string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := h.ownAccount(r)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		itemID, err := uuid.Parse(r.PathValue("itemID"))
		if err != nil {
			http.Error(w, "bad item id", http.StatusBadRequest)
			return
		}
		done, err := op(r.Context(), a.ID, domain.ImportQueueItemID(itemID))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !done {
			http.Error(w, conflictMsg, http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{respKey: true})
	}
}
