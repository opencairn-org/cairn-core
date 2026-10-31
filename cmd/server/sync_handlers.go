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
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
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
	rows, err := h.app.Accounts.List(r.Context(), userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, accountRowJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": out,
		// Instance-wide reconcile cadence — shown next to the per-connection
		// override so "0 = instance default" names a concrete number.
		"default_poll_interval_seconds": int(h.app.DefaultPollInterval.Seconds()),
	})
}

// syncFormatTimePtr formats t as RFC3339 UTC, or nil when unset.
func syncFormatTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// accountRowJSON renders one account with its sync status (GET /api/accounts).
func accountRowJSON(row view.AccountRow) map[string]any {
	a := row.Account
	var connectionID any
	if a.ConnectionID != nil {
		connectionID = a.ConnectionID.String()
	}
	pools := make([]map[string]any, 0, len(row.Pools))
	for _, p := range row.Pools {
		pools = append(pools, map[string]any{
			"name": p.Name, "package": p.Package, "parser_version": p.ParserVersion, "release": p.Release,
			"online": p.Online, "compatible": p.Compatible,
		})
	}
	return map[string]any{
		"id": a.ID.String(), "provider": a.Provider,
		"connection_id":       connectionID,
		"provider_account_id": a.ProviderAccountID,
		"label":               a.DisplayLabel, "status": string(a.Status),
		"imported":              row.Imported,
		"provider_total":        row.ProviderTotal,
		"last_sync_at":          syncFormatTimePtr(a.LastSyncAt),
		"watermark":             syncFormatTimePtr(a.SyncWatermark),
		"rate_limited":          row.RateLimited,
		"rate_limit_budget":     budgetJSON(row.Budget),
		"discovering":           row.Discovering,
		"poll_interval_seconds": row.PollInterval,
		"auto_import_enabled":   a.AutoImportEnabled,
		"queue": map[string]int{
			"pending":     row.Queue[domain.ImportStatusPending],
			"in_progress": row.Queue[domain.ImportStatusInProgress],
			"done":        row.Queue[domain.ImportStatusDone],
			"failed":      row.Queue[domain.ImportStatusFailed],
			"skipped":     row.Queue[domain.ImportStatusSkipped],
		},
		"assigned_worker": a.AssignedWorkerName,
		"worker_pools":    pools,
		"out_of_date":     row.OutOfDate,
	}
}

// budgetJSON renders the live budget; nil when unknown.
func budgetJSON(b domain.RateBudget) any {
	if b.Empty() {
		return nil
	}
	out := map[string]any{}
	for name, w := range map[string]*domain.BudgetWindow{"short": b.Short, "daily": b.Daily} {
		if w != nil {
			out[name] = map[string]any{"used": w.Used, "limit": w.Limit, "resets_at": w.ResetsAt.UTC().Format(time.RFC3339)}
		}
	}
	return out
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
	goSafe(h.logger, "full discovery", func() { runFullDiscovery(h.app, h.logger, a, body.SkipExisting) })
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
	rows, _ := h.app.Accounts.History(r.Context(), a, 0)
	events := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		events = append(events, historyEventJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func historyEventJSON(row view.SyncEventRow) map[string]any {
	e := row.Event
	ev := map[string]any{
		"kind":        e.Kind,
		"count":       e.Count,
		"detail":      e.Detail,
		"external_id": e.ExternalID,
		"occurred_at": e.OccurredAt.UTC().Format(time.RFC3339),
		"status":      row.Status,
	}
	if row.ActivityID != nil {
		ev["activity_id"] = row.ActivityID.String()
	}
	if row.ExternalURL != "" {
		ev["external_url"] = row.ExternalURL
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
	rows, err := h.app.Accounts.QueueItems(r.Context(), a, statuses, limit)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, syncQueueItemRow(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func syncQueueItemRow(r view.QueueItemRow) map[string]any {
	it := r.Item
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
	if r.ExternalURL != "" {
		row["external_url"] = r.ExternalURL
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
