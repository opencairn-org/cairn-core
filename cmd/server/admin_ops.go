package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	natsadapter "github.com/opencairn-org/cairn-core/internal/adapter/secondary/nats"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// adminOpsHandlers holds the dependencies of the admin maintenance routes.
type adminOpsHandlers struct {
	app    *App
	logger *slog.Logger
}

// mountAdminOps wires the operator-only maintenance endpoints under the
// session+admin-gated /api/admin/* surface (resolveAdminUser). These have no
// other home — first-time NATS account-key setup, key rotation, dead-letter
// inspection/replay, and an on-demand geocode backfill. (The legacy token-gated
// /admin/* smoketest layer was removed; user-facing recompute/detach/listings
// now live on the Connect-RPC AdminService + the per-activity /api/* surface.)
func mountAdminOps(mux *http.ServeMux, app *App, logger *slog.Logger) {
	h := &adminOpsHandlers{app: app, logger: logger}

	// On-demand geocode start-location backfill (the continuous backfiller
	// already drains on a timer; this runs one batch immediately). Async.
	mux.HandleFunc("POST /api/admin/geocode/backfill", h.geocodeBackfill)

	// First-time NATS account signing-key setup. Returns the account public key
	// to paste into nats-server.conf auth_callout.issuer.
	mux.HandleFunc("POST /api/admin/nats/bootstrap-account-key", h.bootstrapAccountKey)

	// Rotate the NATS account signing key (mint new active + deactivate old,
	// atomic) and reload the live issuer's cache. Trust BOTH keys during overlap.
	mux.HandleFunc("POST /api/admin/nats/rotate-account-key", h.rotateAccountKey)

	// Rebuild every user's gear totals from live activities (after a
	// migration or a bug in the totals follow-up).
	mux.HandleFunc("POST /api/admin/gear/rebuild-totals", h.rebuildGearTotals)

	// Dead-letter inspection + replay.
	mux.HandleFunc("GET /api/admin/dlq", h.listDLQ)
	mux.HandleFunc("POST /api/admin/dlq/{id}/replay", h.replayDLQ)
	// Job introspection over the import queue, and sources whose worker is gone.
	mux.HandleFunc("GET /api/admin/jobs", h.listJobs)
	mux.HandleFunc("GET /api/admin/jobs/{id}", h.getJob)
	mux.HandleFunc("POST /api/admin/jobs/{id}/retry", h.retryJob)
	mux.HandleFunc("POST /api/admin/jobs/{id}/cancel", h.cancelJob)
	mux.HandleFunc("GET /api/admin/sources/orphaned", h.orphanedSources)

	logger.Info("admin ops endpoints mounted (/api/admin/{geocode,nats,dlq})")
}

// gate writes 403 and returns false unless the caller is an admin.
func (h *adminOpsHandlers) gate(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := resolveAdminUser(r, h.app); !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return false
	}
	return true
}

func (h *adminOpsHandlers) geocodeBackfill(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	admin, _ := resolveAdminUser(r, h.app)
	limit, err := h.app.Ops.RunGeocodeBackfill(r.Context(), restActor(r, admin), parseIntQuery(r, "limit", 50))
	if err != nil {
		writeActionError(w, err, "backfill failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "limit": limit})
}

func (h *adminOpsHandlers) rebuildGearTotals(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	admin, _ := resolveAdminUser(r, h.app)
	n, err := h.app.Ops.RebuildGearTotals(r.Context(), restActor(r, admin))
	if err != nil {
		writeActionError(w, err, "rebuild failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rebuilt": true, "users": n})
}

func (h *adminOpsHandlers) bootstrapAccountKey(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	admin, _ := resolveAdminUser(r, h.app)
	key, created, err := h.app.Ops.BootstrapKey(r.Context(), restActor(r, admin))
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

func (h *adminOpsHandlers) rotateAccountKey(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	admin, _ := resolveAdminUser(r, h.app)
	key, oldPub, err := h.app.Ops.RotateKey(r.Context(), restActor(r, admin))
	if err != nil {
		writeActionError(w, err, "rotate failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"new_public_key": key.PublicKey, "old_public_key": oldPub, "action": "rotated",
		"note": "trust BOTH keys in nats-server.conf during the overlap, then drop the old one once all workers have renewed",
	})
}

// natsKeyManager runs the account-key operations over the nats adapter.
type natsKeyManager struct{ app *App }

func (m natsKeyManager) ready() error {
	if m.app.SigningKeys == nil || m.app.SecretBox == nil {
		return errors.New("signing-key repo / secret box not wired")
	}
	return nil
}

func (m natsKeyManager) Bootstrap(ctx context.Context, by *domain.UserID) (domain.SigningKey, bool, error) {
	if err := m.ready(); err != nil {
		return domain.SigningKey{}, false, err
	}
	if existing, err := m.app.SigningKeys.GetActive(ctx, domain.SigningKeyPurposeNATSAccount); err == nil {
		return existing, false, nil
	}
	key, err := natsadapter.BootstrapAccountKey(ctx, m.app.SigningKeys, m.app.SecretBox, by)
	return key, err == nil, err
}

func (m natsKeyManager) Rotate(ctx context.Context, by *domain.UserID) (domain.SigningKey, string, error) {
	if err := m.ready(); err != nil {
		return domain.SigningKey{}, "", err
	}
	var (
		newKey domain.SigningKey
		oldPub string
	)
	err := m.app.Tx.InTx(ctx, func(ctx context.Context) error {
		var e error
		newKey, oldPub, e = natsadapter.RotateAccountKey(ctx, m.app.SigningKeys, m.app.SecretBox, by)
		return e
	})
	if err != nil {
		return domain.SigningKey{}, "", err
	}
	if m.app.NATSCredentialIssuer != nil {
		m.app.NATSCredentialIssuer.InvalidateCache()
	}
	return newKey, oldPub, nil
}

// geocodeBackfillRunner runs one batch in the background, logging the outcome.
func geocodeBackfillRunner(app *App, logger *slog.Logger) func(limit int) {
	if app.ComputeStartPlace == nil {
		return nil
	}
	return func(limit int) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			n, err := app.ComputeStartPlace.BackfillBatch(ctx, limit)
			if err != nil {
				logger.Warn("admin geocode backfill failed", "error", err)
				return
			}
			logger.Info("admin geocode backfill complete", "resolved", n)
		}()
	}
}

func (h *adminOpsHandlers) listDLQ(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	if h.app.Jobs == nil {
		http.Error(w, "dlq repo not wired", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	list, err := h.app.Jobs.ListDeadLetters(r.Context(), port.DeadLetterFilter{
		Stream:            q.Get("stream"),
		Subject:           q.Get("subject"),
		IncludeReplayed:   q.Get("include_replayed") == "true",
		Limit:             parseIntQuery(r, "limit", 50),
		BeforeFirstSeenAt: parseTimeQuery(q.Get("before")),
	})
	if err != nil {
		writeActionError(w, err, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, j := range list {
		out = append(out, dlqJobToJSON(j))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "count": len(out)})
}

func (h *adminOpsHandlers) replayDLQ(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	if h.app.Jobs == nil {
		http.Error(w, "dlq + nats not wired", http.StatusServiceUnavailable)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	admin, _ := resolveAdminUser(r, h.app)
	_, newMsgID, err := h.app.Jobs.Requeue(r.Context(), restActor(r, admin), domain.DeadLetteredJobID(id))
	if err != nil {
		writeActionError(w, err, "replay failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id.String(), "replayed": true, "new_msg_id": newMsgID})
}

func dlqJobToJSON(j domain.DeadLetteredJob) map[string]any {
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

// enrollmentResponseJSON shapes a WorkerEnrollment for the /api/admin/worker-
// enrollments responses.
func enrollmentResponseJSON(e domain.WorkerEnrollment) map[string]any {
	out := map[string]any{
		"id":                  e.ID.String(),
		"provider":            e.Provider,
		"name":                e.Name,
		"version":             e.Version,
		"worker_key":          e.WorkerKey(),
		"worker_name_pattern": e.WorkerNamePattern,
		"permission_template": e.PermissionTemplate,
		"created_at":          e.CreatedAt.Format(time.RFC3339),
		"note":                e.Note,
		"expires_at":          e.ExpiresAt.Format(time.RFC3339),
		"max_uses":            e.MaxUses,
		"uses":                e.Uses,
		"token_hash_hex":      fmt.Sprintf("%x", e.TokenHash),
		"is_revoked":          e.IsRevoked(),
		"reenroll_required":   e.NeedsReenroll(),
	}
	if e.CreatedByUserID != nil {
		out["created_by_user_id"] = e.CreatedByUserID.String()
	}
	if e.RevokedAt != nil {
		out["revoked_at"] = e.RevokedAt.Format(time.RFC3339)
		out["revoked_reason"] = e.RevokedReason
	}
	if e.RevokedByUserID != nil {
		out["revoked_by_user_id"] = e.RevokedByUserID.String()
	}
	return out
}

// natsReplayer republishes a dead-lettered payload on the replay subject.
type natsReplayer struct{ app *App }

func (n natsReplayer) Replay(ctx context.Context, job domain.DeadLetteredJob) (string, error) {
	if n.app.NATSBus == nil {
		return "", errors.New("message bus not configured")
	}
	newMsgID := fmt.Sprintf("replay:%s:%d", job.ID.String(), time.Now().Unix())
	if err := n.app.NATSBus.Publish(ctx, "cairn.replay.unsorted", newMsgID, job.Payload); err != nil {
		return "", err
	}
	return newMsgID, nil
}

func (h *adminOpsHandlers) jobsReady(w http.ResponseWriter, r *http.Request) bool {
	if !h.gate(w, r) {
		return false
	}
	if h.app.Jobs == nil {
		http.Error(w, "import queue not wired", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (h *adminOpsHandlers) listJobs(w http.ResponseWriter, r *http.Request) {
	if !h.jobsReady(w, r) {
		return
	}
	q := r.URL.Query()
	f := port.ImportQueueFilter{Statuses: csv[domain.ImportItemStatus](q.Get("status")), ItemTypes: csv[domain.ImportItemType](q.Get("type")),
		Providers: csv[string](q.Get("provider")), Limit: parseIntQuery(r, "limit", 50), Offset: parseIntQuery(r, "offset", 0)}
	f.From, f.To = parseTimeQuery(q.Get("from")), parseTimeQuery(q.Get("to"))
	if raw := q.Get("user_id"); raw != "" {
		uid, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "bad user_id", http.StatusBadRequest)
			return
		}
		u := domain.UserID(uid)
		f.UserID = &u
	}
	items, total, counts, err := h.app.Jobs.List(r.Context(), f)
	if err != nil {
		writeActionError(w, err, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, queueItemAdminJSON(it))
	}
	byStatus := map[string]int{}
	for st, n := range counts {
		byStatus[string(st)] = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out, "total": total, "count_by_status": byStatus})
}

func (h *adminOpsHandlers) jobID(w http.ResponseWriter, r *http.Request) (domain.ImportQueueItemID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return domain.ImportQueueItemID{}, false
	}
	return domain.ImportQueueItemID(id), true
}

func (h *adminOpsHandlers) getJob(w http.ResponseWriter, r *http.Request) {
	if !h.jobsReady(w, r) {
		return
	}
	id, ok := h.jobID(w, r)
	if !ok {
		return
	}
	it, err := h.app.Jobs.Get(r.Context(), id)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": queueItemAdminJSON(it)})
}

func (h *adminOpsHandlers) retryJob(w http.ResponseWriter, r *http.Request) {
	if !h.jobsReady(w, r) {
		return
	}
	id, ok := h.jobID(w, r)
	if !ok {
		return
	}
	admin, _ := resolveAdminUser(r, h.app)
	it, err := h.app.Jobs.Retry(r.Context(), restActor(r, admin), id, r.URL.Query().Get("reset") == "true")
	if err != nil {
		writeActionError(w, err, "retry failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": queueItemAdminJSON(it)})
}

func (h *adminOpsHandlers) cancelJob(w http.ResponseWriter, r *http.Request) {
	if !h.jobsReady(w, r) {
		return
	}
	id, ok := h.jobID(w, r)
	if !ok {
		return
	}
	admin, _ := resolveAdminUser(r, h.app)
	it, err := h.app.Jobs.Cancel(r.Context(), restActor(r, admin), id)
	if err != nil {
		writeActionError(w, err, "cancel failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": queueItemAdminJSON(it)})
}

func (h *adminOpsHandlers) orphanedSources(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	if h.app.Orphans == nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	groups, err := h.app.Orphans.List(r.Context(), r.URL.Query().Get("worker"))
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, map[string]any{"worker_name": g.WorkerName, "sources": g.Sources, "external_accounts": g.ExternalAccounts,
			"users": g.Users, "oldest_imported_at": fmtTimePtr(g.OldestImportedAt)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

func queueItemAdminJSON(it domain.ImportQueueItem) map[string]any {
	return map[string]any{
		"id": it.ID.String(), "external_account_id": it.ExternalAccountID.String(), "user_id": it.UserID.String(), "provider": it.Provider,
		"item_type": string(it.ItemType), "external_id": it.ExternalID, "item_time": fmtTimePtr(it.ItemTime), "status": string(it.Status),
		"priority": it.Priority, "attempts": it.Attempts, "last_error": it.LastError, "created_at": it.CreatedAt.UTC().Format(time.RFC3339),
		"started_at": fmtTimePtr(it.StartedAt), "completed_at": fmtTimePtr(it.CompletedAt),
	}
}
