package rest

import (
	"net/http"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/instance"
	"github.com/opencairn-org/cairn-core/internal/usecase/jobs"
	"github.com/opencairn-org/cairn-core/internal/usecase/maintenance"
	"github.com/opencairn-org/cairn-core/internal/usecase/webhooks"
)

// AdminOpsHandlers are the operator-only maintenance endpoints: NATS account
// keys, dead letters, the import queue, orphaned sources, webhook signing
// tokens, instance-wide maintenance and one-shot backfills. A nil optional
// use case answers 503.
//
//	POST /api/admin/geocode/backfill            ?limit=
//	POST /api/admin/nats/bootstrap-account-key
//	POST /api/admin/nats/rotate-account-key
//	POST /api/admin/gear/rebuild-totals
//	GET  /api/admin/dlq                         ?stream=&subject=&include_replayed=&limit=&before=
//	POST /api/admin/dlq/{id}/replay
//	GET  /api/admin/jobs                        ?status=&type=&provider=&user_id=&from=&to=&limit=&offset=
//	GET  /api/admin/jobs/{id}
//	POST /api/admin/jobs/{id}/retry             ?reset=
//	POST /api/admin/jobs/{id}/cancel
//	GET  /api/admin/sources/orphaned            ?worker=
//	GET  /api/admin/webhook-tokens              ?provider=&limit=&offset=
//	POST /api/admin/webhook-tokens/rotate       { provider?, older_than_seconds?, external_account_ids? }
//	POST /api/admin/maintenance                 { operation, user_ids?, confirm_phrase }
//	GET  /api/admin/maintenance/jobs
//	POST /api/admin/maintenance/jobs/{id}/cancel
type AdminOpsHandlers struct {
	Auth          *Auth
	Ops           *instance.Ops
	Jobs          *jobs.Jobs
	Orphans       *jobs.Orphans
	WebhookTokens *webhooks.Tokens
	Maintenance   *maintenance.Runner
}

// Mount registers the routes.
func (h *AdminOpsHandlers) Mount(mux *http.ServeMux) {
	// One geocode batch now; the continuous backfiller drains on a timer.
	mux.HandleFunc("POST /api/admin/geocode/backfill", h.geocodeBackfill)
	// Returns the account public key for nats-server.conf auth_callout.issuer.
	mux.HandleFunc("POST /api/admin/nats/bootstrap-account-key", h.bootstrapAccountKey)
	// New active key, old one deactivated; trust both during the overlap.
	mux.HandleFunc("POST /api/admin/nats/rotate-account-key", h.rotateAccountKey)
	mux.HandleFunc("POST /api/admin/gear/rebuild-totals", h.rebuildGearTotals)
	mux.HandleFunc("GET /api/admin/dlq", h.listDLQ)
	mux.HandleFunc("POST /api/admin/dlq/{id}/replay", h.replayDLQ)
	mux.HandleFunc("GET /api/admin/jobs", h.listJobs)
	mux.HandleFunc("GET /api/admin/jobs/{id}", h.getJob)
	mux.HandleFunc("POST /api/admin/jobs/{id}/retry", h.retryJob)
	mux.HandleFunc("POST /api/admin/jobs/{id}/cancel", h.cancelJob)
	mux.HandleFunc("GET /api/admin/sources/orphaned", h.orphanedSources)
	mux.HandleFunc("GET /api/admin/webhook-tokens", h.listWebhookTokens)
	mux.HandleFunc("POST /api/admin/webhook-tokens/rotate", h.rotateWebhookTokens)
	mux.HandleFunc("POST /api/admin/maintenance", h.triggerMaintenance)
	mux.HandleFunc("GET /api/admin/maintenance/jobs", h.listMaintenanceJobs)
	mux.HandleFunc("POST /api/admin/maintenance/jobs/{id}/cancel", h.cancelMaintenanceJob)
}

// ready gates on admin, then answers 503 when the use case is not wired.
func (h *AdminOpsHandlers) ready(w http.ResponseWriter, r *http.Request, wired bool, msg string) (domain.User, bool) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok {
		return admin, false
	}
	if !wired {
		http.Error(w, msg, http.StatusServiceUnavailable)
		return admin, false
	}
	return admin, true
}

func (h *AdminOpsHandlers) geocodeBackfill(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok {
		return
	}
	limit, err := h.Ops.RunGeocodeBackfill(r.Context(), actor(r, admin), parseIntQuery(r, "limit", 50))
	if err != nil {
		writeActionError(w, err, "backfill failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "limit": limit})
}

func (h *AdminOpsHandlers) rebuildGearTotals(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok {
		return
	}
	n, err := h.Ops.RebuildGearTotals(r.Context(), actor(r, admin))
	if err != nil {
		writeActionError(w, err, "rebuild failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rebuilt": true, "users": n})
}

func (h *AdminOpsHandlers) bootstrapAccountKey(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok {
		return
	}
	key, created, err := h.Ops.BootstrapKey(r.Context(), actor(r, admin))
	if err != nil {
		writeActionError(w, err, "bootstrap failed")
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, map[string]any{
			"public_key": key.PublicKey, "created_at": key.CreatedAt.UTC(), "action": "no_change",
			"note": "active signing key already exists",
		})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"public_key": key.PublicKey, "created_at": key.CreatedAt.UTC(), "action": "created",
		"note": "copy this public key into nats-server.conf auth_callout.issuer",
	})
}

func (h *AdminOpsHandlers) rotateAccountKey(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok {
		return
	}
	key, oldPub, err := h.Ops.RotateKey(r.Context(), actor(r, admin))
	if err != nil {
		writeActionError(w, err, "rotate failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"new_public_key": key.PublicKey, "old_public_key": oldPub, "action": "rotated",
		"note": "trust BOTH keys in nats-server.conf during the overlap, then drop the old one once all workers have renewed",
	})
}

func (h *AdminOpsHandlers) listDLQ(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.ready(w, r, h.Jobs != nil, "dlq repo not wired"); !ok {
		return
	}
	q := r.URL.Query()
	list, err := h.Jobs.ListDeadLetters(r.Context(), port.DeadLetterFilter{
		Stream:            q.Get("stream"),
		Subject:           q.Get("subject"),
		IncludeReplayed:   q.Get("include_replayed") == "true",
		Limit:             parseIntQuery(r, "limit", 50),
		BeforeFirstSeenAt: adminParseTime(q.Get("before")),
	})
	if err != nil {
		writeActionError(w, err, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, j := range list {
		out = append(out, adminDeadLetterJSON(j))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "count": len(out)})
}

func (h *AdminOpsHandlers) replayDLQ(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.ready(w, r, h.Jobs != nil, "dlq + nats not wired")
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.DeadLetteredJobID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	_, newMsgID, err := h.Jobs.Requeue(r.Context(), actor(r, admin), id)
	if err != nil {
		writeActionError(w, err, "replay failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id.String(), "replayed": true, "new_msg_id": newMsgID})
}

func adminDeadLetterJSON(j domain.DeadLetteredJob) map[string]any {
	out := map[string]any{
		"id":              j.ID.String(),
		"stream":          j.Stream,
		"subject":         j.Subject,
		"consumer":        j.Consumer,
		"msg_id":          j.MsgID,
		"delivered_count": j.DeliveredCount,
		"last_error":      j.LastError,
		"first_seen_at":   j.FirstSeenAt.Format(time.RFC3339),
		"last_seen_at":    j.LastSeenAt.Format(time.RFC3339),
		"replay_count":    j.ReplayCount,
	}
	if j.ReplayedAt != nil {
		out["replayed_at"] = j.ReplayedAt.Format(time.RFC3339)
	}
	return out
}

func (h *AdminOpsHandlers) jobsReady(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	return h.ready(w, r, h.Jobs != nil, "import queue not wired")
}

func (h *AdminOpsHandlers) listJobs(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.jobsReady(w, r); !ok {
		return
	}
	q := r.URL.Query()
	f := port.ImportQueueFilter{Statuses: adminCSV[domain.ImportItemStatus](q.Get("status")), ItemTypes: adminCSV[domain.ImportItemType](q.Get("type")),
		Providers: adminCSV[string](q.Get("provider")), Limit: parseIntQuery(r, "limit", 50), Offset: parseIntQuery(r, "offset", 0)}
	f.From, f.To = adminParseTime(q.Get("from")), adminParseTime(q.Get("to"))
	if raw := q.Get("user_id"); raw != "" {
		uid, err := domain.ParseUUID[domain.UserID](raw)
		if err != nil {
			http.Error(w, "bad user_id", http.StatusBadRequest)
			return
		}
		f.UserID = &uid
	}
	items, total, counts, err := h.Jobs.List(r.Context(), f)
	if err != nil {
		writeActionError(w, err, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, adminQueueItemJSON(it))
	}
	byStatus := map[string]int{}
	for st, n := range counts {
		byStatus[string(st)] = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out, "total": total, "count_by_status": byStatus})
}

// job gates on the import queue and parses the {id} job.
func (h *AdminOpsHandlers) job(w http.ResponseWriter, r *http.Request) (domain.User, domain.ImportQueueItemID, bool) {
	admin, ok := h.jobsReady(w, r)
	if !ok {
		return admin, domain.ImportQueueItemID{}, false
	}
	id, ok := adminParseID[domain.ImportQueueItemID](w, r)
	return admin, id, ok
}

func (h *AdminOpsHandlers) getJob(w http.ResponseWriter, r *http.Request) {
	_, id, ok := h.job(w, r)
	if !ok {
		return
	}
	it, err := h.Jobs.Get(r.Context(), id)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": adminQueueItemJSON(it)})
}

func (h *AdminOpsHandlers) retryJob(w http.ResponseWriter, r *http.Request) {
	admin, id, ok := h.job(w, r)
	if !ok {
		return
	}
	it, err := h.Jobs.Retry(r.Context(), actor(r, admin), id, r.URL.Query().Get("reset") == "true")
	if err != nil {
		writeActionError(w, err, "retry failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": adminQueueItemJSON(it)})
}

func (h *AdminOpsHandlers) cancelJob(w http.ResponseWriter, r *http.Request) {
	admin, id, ok := h.job(w, r)
	if !ok {
		return
	}
	it, err := h.Jobs.Cancel(r.Context(), actor(r, admin), id)
	if err != nil {
		writeActionError(w, err, "cancel failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": adminQueueItemJSON(it)})
}

func (h *AdminOpsHandlers) orphanedSources(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.ready(w, r, h.Orphans != nil, "unavailable"); !ok {
		return
	}
	groups, err := h.Orphans.List(r.Context(), r.URL.Query().Get("worker"))
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, map[string]any{"worker_name": g.WorkerName, "sources": g.Sources, "external_accounts": g.ExternalAccounts,
			"users": g.Users, "oldest_imported_at": adminTimePtr(g.OldestImportedAt)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

func adminQueueItemJSON(it domain.ImportQueueItem) map[string]any {
	return map[string]any{
		"id": it.ID.String(), "external_account_id": it.ExternalAccountID.String(), "user_id": it.UserID.String(), "provider": it.Provider,
		"item_type": string(it.ItemType), "external_id": it.ExternalID, "item_time": adminTimePtr(it.ItemTime), "status": string(it.Status),
		"priority": it.Priority, "attempts": it.Attempts, "last_error": it.LastError, "created_at": it.CreatedAt.UTC().Format(time.RFC3339),
		"started_at": adminTimePtr(it.StartedAt), "completed_at": adminTimePtr(it.CompletedAt),
	}
}

func (h *AdminOpsHandlers) listWebhookTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.ready(w, r, h.WebhookTokens != nil, "webhook tokens not wired"); !ok {
		return
	}
	rows, total, err := h.WebhookTokens.List(r.Context(), r.URL.Query().Get("provider"), parseIntQuery(r, "limit", 50), parseIntQuery(r, "offset", 0))
	if err != nil {
		writeActionError(w, err, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := map[string]any{"external_account_id": row.ExternalAccountID.String(), "user_id": row.UserID.String(), "provider": row.Provider, "last_rotated_at": nil}
		if row.RotatedAt != nil {
			item["last_rotated_at"] = row.RotatedAt.UTC()
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out, "total": total})
}

func (h *AdminOpsHandlers) rotateWebhookTokens(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.ready(w, r, h.WebhookTokens != nil, "webhook tokens not wired")
	if !ok {
		return
	}
	var body struct {
		Provider         string   `json:"provider"`
		OlderThanSeconds *int64   `json:"older_than_seconds"`
		AccountIDs       []string `json:"external_account_ids"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	sel := domain.WebhookRotationSelector{Provider: body.Provider}
	if body.OlderThanSeconds != nil {
		d := time.Duration(*body.OlderThanSeconds) * time.Second
		sel.OlderThan = &d
	}
	for _, raw := range body.AccountIDs {
		id, err := domain.ParseUUID[domain.ExternalAccountID](raw)
		if err != nil {
			http.Error(w, "bad external_account_id", http.StatusBadRequest)
			return
		}
		sel.AccountIDs = append(sel.AccountIDs, id)
	}
	rotated, err := h.WebhookTokens.BulkRotate(r.Context(), actor(r, admin), sel)
	if err != nil {
		writeActionError(w, err, "rotate failed")
		return
	}
	ids := make([]string, 0, len(rotated))
	for _, id := range rotated {
		ids = append(ids, id.String())
	}
	writeJSON(w, http.StatusOK, map[string]any{"rotated_count": len(rotated), "external_account_ids": ids})
}

func (h *AdminOpsHandlers) maintenanceReady(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	return h.ready(w, r, h.Maintenance != nil, "maintenance not wired")
}

func (h *AdminOpsHandlers) triggerMaintenance(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.maintenanceReady(w, r)
	if !ok {
		return
	}
	var body struct {
		Operation     string   `json:"operation"`
		UserIDs       []string `json:"user_ids"`
		ConfirmPhrase string   `json:"confirm_phrase"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	users := make([]domain.UserID, 0, len(body.UserIDs))
	for _, raw := range body.UserIDs {
		uid, err := domain.ParseUUID[domain.UserID](raw)
		if err != nil {
			http.Error(w, "bad user_id", http.StatusBadRequest)
			return
		}
		users = append(users, uid)
	}
	job, err := h.Maintenance.Trigger(r.Context(), actor(r, admin), domain.MaintenanceOperation(body.Operation), users, body.ConfirmPhrase)
	if err != nil {
		writeActionError(w, err, "trigger failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "estimated_record_count": job.Estimated, "job": adminMaintenanceJobJSON(job)})
}

func (h *AdminOpsHandlers) listMaintenanceJobs(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.maintenanceReady(w, r); !ok {
		return
	}
	list := h.Maintenance.List(r.Context())
	out := make([]map[string]any, 0, len(list))
	for _, job := range list {
		out = append(out, adminMaintenanceJobJSON(job))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (h *AdminOpsHandlers) cancelMaintenanceJob(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.maintenanceReady(w, r)
	if !ok {
		return
	}
	job, err := h.Maintenance.Cancel(r.Context(), actor(r, admin), r.PathValue("id"))
	if err != nil {
		writeActionError(w, err, "cancel failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": adminMaintenanceJobJSON(job)})
}

func adminMaintenanceJobJSON(job domain.MaintenanceJob) map[string]any {
	users := make([]string, 0, len(job.UserIDs))
	for _, uid := range job.UserIDs {
		users = append(users, uid.String())
	}
	out := map[string]any{
		"id": job.ID, "operation": string(job.Operation), "user_ids": users, "status": string(job.Status),
		"estimated_record_count": job.Estimated, "processed_count": job.Processed, "failed_count": job.Failed,
		"error_message": job.Error, "queued_at": job.QueuedAt, "started_at": job.StartedAt, "finished_at": job.FinishedAt,
		"triggered_by_user_id": nil,
	}
	if job.TriggeredBy != nil {
		out["triggered_by_user_id"] = job.TriggeredBy.String()
	}
	return out
}
