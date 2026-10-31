package rest

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	syncuc "github.com/opencairn-org/cairn-core/internal/usecase/sync"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// SyncHandlers serve a user's provider connections: status, full sync,
// single imports, the import queue and per-connection settings.
//
//	GET  /api/accounts                                          accounts + queue
//	PUT  /api/accounts/{id}/auto-import                         {enabled} (#93)
//	PUT  /api/accounts/{id}/worker                              {worker, confirm} (docs/adr/0006)
//	POST /api/accounts/{id}/reparse-out-of-date                 bulk re-parse (docs/adr/0005)
//	POST /api/accounts/{id}/sync/preview                        discover + summarize
//	POST /api/accounts/{id}/sync/start                          {skip_existing}: discover + enqueue
//	POST /api/accounts/{id}/import-one                          {external_id}
//	PUT  /api/accounts/{id}/config                              {poll_interval_seconds}
//	GET  /api/accounts/{id}/history                             import history
//	GET  /api/accounts/{id}/queue                               queue counts
//	GET  /api/accounts/{id}/queue/items?status=&limit=          queue rows
//	POST /api/accounts/{id}/queue/items/{itemID}/requeue        failed → pending
//	POST /api/accounts/{id}/queue/items/{itemID}/move-to-top    claim next
type SyncHandlers struct {
	Auth     *Auth
	Accounts *view.Accounts
	Imports  *syncuc.Imports
	// DefaultPollSeconds is the instance reconcile cadence, shown next to the override.
	DefaultPollSeconds int
	Log                *slog.Logger
}

// Mount registers the routes.
func (h *SyncHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/accounts", h.list)
	mux.HandleFunc("PUT /api/accounts/{id}/auto-import", h.setAutoImport)
	mux.HandleFunc("PUT /api/accounts/{id}/worker", h.setWorker)
	mux.HandleFunc("POST /api/accounts/{id}/reparse-out-of-date", h.reparseOutOfDate)
	mux.HandleFunc("POST /api/accounts/{id}/sync/preview", h.preview)
	mux.HandleFunc("POST /api/accounts/{id}/sync/start", h.start)
	mux.HandleFunc("POST /api/accounts/{id}/import-one", h.importOne)
	mux.HandleFunc("PUT /api/accounts/{id}/config", h.putConfig)
	mux.HandleFunc("GET /api/accounts/{id}/history", h.history)
	mux.HandleFunc("GET /api/accounts/{id}/queue", h.queue)
	mux.HandleFunc("GET /api/accounts/{id}/queue/items", h.queueItems)
	mux.HandleFunc("POST /api/accounts/{id}/queue/items/{itemID}/requeue",
		h.queueItemAction(h.Imports.RequeueFailed, "item is not in failed state", "requeued"))
	mux.HandleFunc("POST /api/accounts/{id}/queue/items/{itemID}/move-to-top",
		h.queueItemAction(h.Imports.MoveToTop, "item is not in pending state", "moved"))
}

// owned resolves the caller's {id} account, writing 404 for anything else
// (anonymous included, so ids don't leak).
func (h *SyncHandlers) owned(w http.ResponseWriter, r *http.Request) (domain.ExternalAccount, bool) {
	uid, ok := h.Auth.User(r)
	if ok {
		if id, err := domain.ParseUUID[domain.ExternalAccountID](r.PathValue("id")); err == nil {
			if a, err := h.Accounts.Owned(r.Context(), uid, id); err == nil {
				return a, true
			}
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
	return domain.ExternalAccount{}, false
}

func (h *SyncHandlers) list(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	rows, err := h.Accounts.List(r.Context(), uid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, syncAccountRowJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":                      out,
		"default_poll_interval_seconds": h.DefaultPollSeconds,
	})
}

// syncTimePtr formats t as RFC3339 UTC, or nil when unset.
func syncTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// syncQueueJSON renders per-status queue counts.
func syncQueueJSON(c map[domain.ImportItemStatus]int) map[string]int {
	return map[string]int{
		"pending":     c[domain.ImportStatusPending],
		"in_progress": c[domain.ImportStatusInProgress],
		"done":        c[domain.ImportStatusDone],
		"failed":      c[domain.ImportStatusFailed],
		"skipped":     c[domain.ImportStatusSkipped],
	}
}

// syncAccountRowJSON renders one account with its sync status.
func syncAccountRowJSON(row view.AccountRow) map[string]any {
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
		"connection_id":         connectionID,
		"provider_account_id":   a.ProviderAccountID,
		"label":                 a.DisplayLabel,
		"status":                string(a.Status),
		"imported":              row.Imported,
		"provider_total":        row.ProviderTotal,
		"last_sync_at":          syncTimePtr(a.LastSyncAt),
		"watermark":             syncTimePtr(a.SyncWatermark),
		"rate_limited":          row.RateLimited,
		"rate_limit_budget":     syncBudgetJSON(row.Budget),
		"discovering":           row.Discovering,
		"poll_interval_seconds": row.PollInterval,
		"auto_import_enabled":   a.AutoImportEnabled,
		"queue":                 syncQueueJSON(row.Queue),
		"assigned_worker":       a.AssignedWorkerName,
		"worker_pools":          pools,
		"out_of_date":           row.OutOfDate,
	}
}

// syncBudgetJSON renders the live budget; nil when unknown.
func syncBudgetJSON(b domain.RateBudget) any {
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

func (h *SyncHandlers) setWorker(w http.ResponseWriter, r *http.Request) {
	a, ok := h.owned(w, r)
	if !ok {
		return
	}
	var body struct {
		Worker string `json:"worker"`
		// Confirm accepts moving to another implementation (sources get re-fetched).
		Confirm bool `json:"confirm"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	flagged, err := h.Imports.SetWorker(r.Context(), a, body.Worker, body.Confirm)
	if err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assigned_worker": body.Worker, "pool_changed": flagged})
}

func (h *SyncHandlers) reparseOutOfDate(w http.ResponseWriter, r *http.Request) {
	a, ok := h.owned(w, r)
	if !ok {
		return
	}
	res, err := h.Imports.ReparseOutOfDate(r.Context(), a)
	if err != nil {
		http.Error(w, "could not list sources", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": res.Queued, "skipped": res.Skipped, "remaining": res.Remaining})
}

func (h *SyncHandlers) setAutoImport(w http.ResponseWriter, r *http.Request) {
	a, ok := h.owned(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.Imports.SetAutoImport(r.Context(), a.ID, body.Enabled); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auto_import_enabled": body.Enabled})
}

func (h *SyncHandlers) preview(w http.ResponseWriter, r *http.Request) {
	a, ok := h.owned(w, r)
	if !ok {
		return
	}
	p, err := h.Imports.Preview(r.Context(), a)
	if err != nil {
		var rl *syncuc.RateLimitedError
		if errors.As(err, &rl) {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error":   "rate_limited",
				"message": "Provider daily/short budget exhausted — no new activities can be fetched right now. Try again later.",
				"until":   rl.Until.Format(time.RFC3339),
			})
			return
		}
		http.Error(w, "discover failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": p.Total, "already_present": p.AlreadyPresent, "new": p.Total - p.AlreadyPresent,
		"complete": p.Complete,
	})
}

// start returns at once; the UI follows the queue counts and "discovering".
func (h *SyncHandlers) start(w http.ResponseWriter, r *http.Request) {
	a, ok := h.owned(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		SkipExisting bool `json:"skip_existing"`
	}
	_ = decodeJSONBody(r, &body)
	h.Imports.StartFullSync(a, body.SkipExisting)
	writeJSON(w, http.StatusOK, map[string]any{"started": true})
}

func (h *SyncHandlers) importOne(w http.ResponseWriter, r *http.Request) {
	a, ok := h.owned(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		ExternalID string `json:"external_id"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	extID := strings.TrimSpace(body.ExternalID)
	if extID == "" {
		http.Error(w, "external_id required", http.StatusBadRequest)
		return
	}
	queued, err := h.Imports.ImportOne(r.Context(), a, extID)
	if err != nil {
		http.Error(w, "enqueue failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": queued, "already_queued": !queued})
}

func (h *SyncHandlers) putConfig(w http.ResponseWriter, r *http.Request) {
	a, ok := h.owned(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		PollIntervalSeconds *int `json:"poll_interval_seconds"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if body.PollIntervalSeconds != nil && *body.PollIntervalSeconds < 0 {
		http.Error(w, "poll_interval_seconds must be >= 0", http.StatusBadRequest)
		return
	}
	if err := h.Imports.SetPollInterval(r.Context(), a.ID, body.PollIntervalSeconds); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *SyncHandlers) history(w http.ResponseWriter, r *http.Request) {
	a, ok := h.owned(w, r)
	if !ok {
		return
	}
	rows, _ := h.Accounts.History(r.Context(), a, 0)
	events := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		events = append(events, syncHistoryEventJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func syncHistoryEventJSON(row view.SyncEventRow) map[string]any {
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

func (h *SyncHandlers) queue(w http.ResponseWriter, r *http.Request) {
	a, ok := h.owned(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, syncQueueJSON(h.Imports.QueueCounts(r.Context(), a.ID)))
}

// syncQueueItemsStatuses parses ?status= (default: everything not done; "all" = no filter).
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

func (h *SyncHandlers) queueItems(w http.ResponseWriter, r *http.Request) {
	a, ok := h.owned(w, r)
	if !ok {
		return
	}
	statuses := syncQueueItemsStatuses(r.URL.Query().Get("status"))
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	rows, err := h.Accounts.QueueItems(r.Context(), a, statuses, limit)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, syncQueueItemJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func syncQueueItemJSON(r view.QueueItemRow) map[string]any {
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
func (h *SyncHandlers) queueItemAction(
	op func(ctx context.Context, id domain.ExternalAccountID, itemID domain.ImportQueueItemID) (bool, error),
	conflictMsg, respKey string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := h.owned(w, r)
		if !ok {
			return
		}
		itemID, err := domain.ParseUUID[domain.ImportQueueItemID](r.PathValue("itemID"))
		if err != nil {
			http.Error(w, "bad item id", http.StatusBadRequest)
			return
		}
		done, err := op(r.Context(), a.ID, itemID)
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
