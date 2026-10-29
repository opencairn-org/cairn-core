package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/usecase/fleet"
)

// admin_api exposes session-authenticated, admin-gated JSON endpoints for the
// admin UI (distinct from the bearer-token /admin/* smoketest layer). The SPA
// is logged in via the session cookie, so these check the session user's role.
//
//	GET  /api/admin/worker-enrollments
//	POST /api/admin/worker-enrollments          { name, provider, expires_in_hours?, note? }
//	POST /api/admin/worker-enrollments/{id}/revoke   { reason? }

// resolveAdminUser resolves the session user and confirms they're an admin.
func resolveAdminUser(r *http.Request, app *App) (domain.User, bool) {
	userID, ok := resolveSessionUser(r, app)
	if !ok {
		return domain.User{}, false
	}
	u, err := app.Users.GetUser(r.Context(), userID)
	if err != nil || u.Role != domain.UserRoleAdmin {
		return domain.User{}, false
	}
	return u, true
}

// resolveModeratorUser admits admins AND moderators — the gate for delegated
// moderation tooling (the report queue, hide-activity).
func resolveModeratorUser(r *http.Request, app *App) (domain.User, bool) {
	userID, ok := resolveSessionUser(r, app)
	if !ok {
		return domain.User{}, false
	}
	u, err := app.Users.GetUser(r.Context(), userID)
	if err != nil || !u.Role.CanModerate() {
		return domain.User{}, false
	}
	return u, true
}

// presenceStaleAfter is how long after a worker's last heartbeat its presence
// record is treated as a ghost (crashed/replaced instance) and pruned. The
// worker heartbeats every ~20s; this is ~2 missed beats of tolerance, well
// under the 60s presence-KV TTL.
const presenceStaleAfter = 45 * time.Second

// adminAPIHandlers holds the dependencies of the session-gated admin routes.
type adminAPIHandlers struct {
	app    *App
	logger *slog.Logger
}

// adminAPIHeartbeat is the worker presence record stored in the presence KV.
type adminAPIHeartbeat struct {
	WorkerName   string                           `json:"worker_name"`
	WorkerKey    string                           `json:"worker_key"`
	InstanceID   string                           `json:"instance_id"`
	Version      string                           `json:"version"`
	Release      string                           `json:"release"`
	Provider     string                           `json:"provider"`
	Package      string                           `json:"package"`
	GoVersion    string                           `json:"go_version"`
	Commit       string                           `json:"commit"`
	BuildDate    string                           `json:"build_date"`
	Webhooks     bool                             `json:"webhooks"`
	Capabilities map[string]capability.Capability `json:"capabilities"`
	LastSeen     string                           `json:"last_seen"`
}

func mountAdminAPI(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Fleet == nil || app.CreateWorkerEnrollment == nil || app.WorkerEnrollments == nil {
		logger.Info("admin api not mounted: worker enrollment not wired")
		return
	}
	h := &adminAPIHandlers{app: app, logger: logger}

	// GET /api/admin/workers — connected workers (from the presence KV), with
	// the version they run and the service type (provider) they connect.
	mux.HandleFunc("GET /api/admin/workers", h.listWorkers)
	// Worker pools (docs/adr/0006): the registry, with retirement.
	mux.HandleFunc("GET /api/admin/worker-pools", h.listPools)
	mux.HandleFunc("POST /api/admin/worker-pools/{provider}/{name}/retire", h.retirePool)
	mux.HandleFunc("GET /api/admin/worker-enrollments", h.listEnrollments)
	mux.HandleFunc("POST /api/admin/worker-enrollments", h.createEnrollment)
	mux.HandleFunc("POST /api/admin/worker-enrollments/{id}/revoke", h.revokeEnrollment)
	// Prolong: extend an enrollment's expiry (works even after it lapsed).
	mux.HandleFunc("POST /api/admin/worker-enrollments/{id}/prolong", h.prolongEnrollment)

	logger.Info("admin api endpoints mounted (session+admin gated)")
}

func (h *adminAPIHandlers) listWorkers(w http.ResponseWriter, r *http.Request) {
	if _, ok := resolveAdminUser(r, h.app); !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	out := []map[string]any{}
	for _, hb := range h.app.Fleet.Workers(r.Context()) {
		out = append(out, adminAPIWorkerJSON(hb))
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": out})
}

func (h *adminAPIHandlers) listPools(w http.ResponseWriter, r *http.Request) {
	if _, ok := resolveAdminUser(r, h.app); !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	pools, err := h.app.Fleet.PoolStatuses(r.Context())
	if err != nil {
		writeActionError(w, err, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(pools))
	for _, p := range pools {
		out = append(out, poolJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": out})
}

func poolJSON(p domain.PoolStatus) map[string]any {
	var retired any
	if p.RetiredAt != nil {
		retired = p.RetiredAt.UTC()
	}
	return map[string]any{
		"provider": p.Provider, "name": p.Name, "package": p.Package,
		"first_seen_at": p.FirstSeenAt.UTC(), "last_seen_at": p.LastSeenAt.UTC(),
		"retired_at": retired, "online_instances": p.OnlineInstances,
	}
}

func (h *adminAPIHandlers) retirePool(w http.ResponseWriter, r *http.Request) {
	admin, ok := resolveAdminUser(r, h.app)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	provider, name := r.PathValue("provider"), r.PathValue("name")
	if err := h.app.Fleet.RetirePool(r.Context(), restActor(r, admin), provider, name); err != nil {
		writeActionError(w, err, "retire failed")
		return
	}
	h.logger.Info("worker pool retired", "provider", provider, "pool", name)
	writeJSON(w, http.StatusOK, map[string]any{"retired": true})
}

// scanWorkerHeartbeats reads every live presence record, pruning stale ones.
func scanWorkerHeartbeats(ctx context.Context, app *App, logger *slog.Logger) []domain.WorkerHeartbeat {
	out := []domain.WorkerHeartbeat{}
	if app.NATSBus == nil {
		return out
	}
	kv, err := app.NATSBus.KV("cairn_worker_presence")
	if err != nil {
		return out
	}
	keys, _ := kv.Keys(ctx)
	now := time.Now()
	for _, k := range keys {
		entry, err := kv.Get(ctx, k)
		if err != nil {
			logger.Debug("admin workers: presence key vanished", "key", k, "error", err)
			continue
		}
		var hb adminAPIHeartbeat
		if json.Unmarshal(entry.Value, &hb) != nil {
			continue
		}
		seen, perr := time.Parse(time.RFC3339, hb.LastSeen)
		if perr == nil && now.Sub(seen) > presenceStaleAfter {
			_ = kv.Delete(ctx, k)
			continue
		}
		out = append(out, heartbeatToDomain(hb, seen))
	}
	return out
}

func heartbeatToDomain(hb adminAPIHeartbeat, seen time.Time) domain.WorkerHeartbeat {
	compatible, known := domain.ReleaseCompatible(Version, hb.Release)
	out := domain.WorkerHeartbeat{
		WorkerName: hb.WorkerName, WorkerKey: hb.WorkerKey, InstanceID: hb.InstanceID, Version: hb.Version, Release: hb.Release,
		Compatible: compatible, Known: known, Provider: hb.Provider, Package: hb.Package, GoVersion: hb.GoVersion,
		Commit: hb.Commit, BuildDate: hb.BuildDate, Webhooks: hb.Webhooks, Capabilities: map[string]domain.LiveCapability{}, LastSeen: seen,
	}
	for k, c := range hb.Capabilities {
		out.Capabilities[k] = domain.LiveCapability{Read: c.Read, Write: c.Write, Backfill: c.Backfill, Granularity: c.Granularity}
	}
	return out
}

func adminAPIWorkerJSON(hb domain.WorkerHeartbeat) map[string]any {
	caps := map[string]map[string]any{}
	for k, c := range hb.Capabilities {
		caps[k] = map[string]any{"read": c.Read, "write": c.Write, "backfill": c.Backfill, "granularity": c.Granularity}
	}
	lastSeen := ""
	if !hb.LastSeen.IsZero() {
		lastSeen = hb.LastSeen.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"worker_name": hb.WorkerName, "worker_key": hb.WorkerKey, "instance_id": hb.InstanceID, "version": hb.Version,
		"release": hb.Release, "release_compatible": hb.Compatible, "release_known": hb.Known, "core_release": Version,
		"provider": hb.Provider, "package": hb.Package, "go_version": hb.GoVersion, "commit": hb.Commit, "build_date": hb.BuildDate,
		"webhooks": hb.Webhooks, "capabilities": caps, "last_seen": lastSeen,
	}
}

func (h *adminAPIHandlers) listEnrollments(w http.ResponseWriter, r *http.Request) {
	if _, ok := resolveAdminUser(r, h.app); !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	list, err := h.app.Fleet.ListEnrollments(r.Context(), r.URL.Query().Get("provider"), true, true)
	if err != nil {
		writeActionError(w, err, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, enrollmentResponseJSON(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"enrollments": out, "providers": enrollmentProviders()})
}

func enrollmentProviders() []string {
	return slices.Sorted(maps.Keys(providerRegistry))
}

func (h *adminAPIHandlers) createEnrollment(w http.ResponseWriter, r *http.Request) {
	admin, ok := resolveAdminUser(r, h.app)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var body struct {
		Name           string `json:"name"`
		Provider       string `json:"provider"`
		ExpiresInHours int    `json:"expires_in_hours"`
		Note           string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	e, token, err := h.app.Fleet.CreateEnrollment(r.Context(), restActor(r, admin), fleet.EnrollmentInput{
		Name: body.Name, Provider: body.Provider, ExpiresIn: time.Duration(body.ExpiresInHours) * time.Hour, Note: body.Note,
	})
	if err != nil {
		writeActionError(w, err, "create enrollment failed")
		return
	}
	h.logger.Info("worker enrollment created via admin UI", "admin", admin.ID, "name", body.Name, "provider", body.Provider)
	writeJSON(w, http.StatusCreated, map[string]any{
		"enrollment": enrollmentResponseJSON(e),
		"token":      token, // shown once
		"warning":    "store this token now — it will not be shown again",
	})
}

func (h *adminAPIHandlers) enrollmentID(w http.ResponseWriter, r *http.Request) (domain.User, domain.WorkerEnrollmentID, bool) {
	admin, ok := resolveAdminUser(r, h.app)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return domain.User{}, domain.WorkerEnrollmentID{}, false
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return domain.User{}, domain.WorkerEnrollmentID{}, false
	}
	return admin, domain.WorkerEnrollmentID(id), true
}

func (h *adminAPIHandlers) revokeEnrollment(w http.ResponseWriter, r *http.Request) {
	admin, id, ok := h.enrollmentID(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeOptionalJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.app.Fleet.RevokeEnrollment(r.Context(), restActor(r, admin), id, body.Reason); err != nil {
		writeActionError(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *adminAPIHandlers) prolongEnrollment(w http.ResponseWriter, r *http.Request) {
	admin, id, ok := h.enrollmentID(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpiresInHours int `json:"expires_in_hours"`
	}
	if err := decodeOptionalJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	expires, err := h.app.Fleet.ProlongEnrollment(r.Context(), restActor(r, admin), id, time.Duration(body.ExpiresInHours)*time.Hour)
	if err != nil {
		writeActionError(w, err, "prolong failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": expires.Format(time.RFC3339)})
}
