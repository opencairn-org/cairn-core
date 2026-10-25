package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/enrollment"
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
	if app.CreateWorkerEnrollment == nil || app.WorkerEnrollments == nil {
		logger.Info("admin api not mounted: worker enrollment not wired")
		return
	}
	h := &adminAPIHandlers{app: app, logger: logger}

	// GET /api/admin/workers — connected workers (from the presence KV), with
	// the version they run and the service type (provider) they connect.
	mux.HandleFunc("GET /api/admin/workers", h.listWorkers)
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
	if h.app.NATSBus != nil {
		if kv, err := h.app.NATSBus.KV("cairn_worker_presence"); err == nil {
			out = h.liveWorkers(r, kv, out)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": out})
}

// liveWorkers appends every live presence record in kv to out, pruning stale ones.
func (h *adminAPIHandlers) liveWorkers(r *http.Request, kv port.KV, out []map[string]any) []map[string]any {
	keys, _ := kv.Keys(r.Context())
	now := time.Now()
	for _, k := range keys {
		entry, err := kv.Get(r.Context(), k)
		if err != nil {
			// Presence keys carry a TTL; one can expire between Keys and Get.
			h.logger.Debug("admin workers: presence key vanished", "key", k, "error", err)
			continue
		}
		var hb adminAPIHeartbeat
		if json.Unmarshal(entry.Value, &hb) != nil {
			continue
		}
		// Only surface LIVE workers. A presence key whose last heartbeat
		// is older than the staleness window is a ghost (a crashed or
		// just-replaced instance — its key lingers until the 60s KV TTL).
		// Skip it AND actively prune it so the admin view cleans up old
		// workers instead of showing duplicates as "online".
		if ts, perr := time.Parse(time.RFC3339, hb.LastSeen); perr == nil {
			if now.Sub(ts) > presenceStaleAfter {
				_ = kv.Delete(r.Context(), k)
				continue
			}
		}
		out = append(out, adminAPIWorkerJSON(hb))
	}
	return out
}

// adminAPIWorkerJSON renders one presence record for the admin UI.
func adminAPIWorkerJSON(hb adminAPIHeartbeat) map[string]any {
	compatible, known := domain.ReleaseCompatible(Version, hb.Release)
	return map[string]any{
		"worker_name": hb.WorkerName,
		"worker_key":  hb.WorkerKey,
		"instance_id": hb.InstanceID,
		"version":     hb.Version,
		// Release vs core's major.minor (docs/adr/0005); release_known is
		// false for untagged builds on either side.
		"release":            hb.Release,
		"release_compatible": compatible,
		"release_known":      known,
		"core_release":       Version,
		"provider":           hb.Provider,
		"package":            hb.Package,
		// Build info is informational (may differ across pooled
		// instances); version+name+provider are the identity.
		"go_version": hb.GoVersion,
		"commit":     hb.Commit,
		"build_date": hb.BuildDate,
		// The worker advertises whether it owns its webhook subject;
		// the UI surfaces the webhook URL only for webhook-capable
		// workers.
		"webhooks": hb.Webhooks,
		// Per-data-type capability manifest (read/write/backfill).
		// Provider-neutral; drives the "this provider gives you …"
		// display and capability-aware sync routing.
		"capabilities": hb.Capabilities,
		"last_seen":    hb.LastSeen,
	}
}

func (h *adminAPIHandlers) listEnrollments(w http.ResponseWriter, r *http.Request) {
	if _, ok := resolveAdminUser(r, h.app); !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	list, err := h.app.WorkerEnrollments.ListEnrollments(r.Context(), port.ListEnrollmentsFilter{
		IncludeRevoked: true,
		IncludeExpired: true,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, enrollmentResponseJSON(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"enrollments": out, "providers": enrollmentProviders()})
}

// enrollmentProviders lists the built-in providers as suggestions; any well-formed provider id is accepted.
func enrollmentProviders() []string {
	return slices.Sorted(maps.Keys(providerRegistry))
}

func (h *adminAPIHandlers) createEnrollment(w http.ResponseWriter, r *http.Request) {
	admin, ok := resolveAdminUser(r, h.app)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	// The provider scopes the worker's NATS permissions, so it is admin-set; the use case format-checks it.
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
	if body.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if body.ExpiresInHours <= 0 {
		body.ExpiresInHours = 365 * 24 // default 365 days (the use-case cap)
	}
	result, err := h.app.CreateWorkerEnrollment.Execute(r.Context(), enrollment.CreateEnrollmentInput{
		Name:      body.Name,
		Provider:  body.Provider,
		ExpiresIn: time.Duration(body.ExpiresInHours) * time.Hour,
		Note:      body.Note,
	})
	if errors.Is(err, enrollment.ErrInvalidInput) {
		http.Error(w, strings.TrimPrefix(err.Error(), enrollment.ErrInvalidInput.Error()+": "), http.StatusBadRequest)
		return
	}
	if err != nil {
		h.logger.Error("create enrollment failed", "error", err)
		http.Error(w, "create enrollment failed", http.StatusInternalServerError)
		return
	}
	h.logger.Info("worker enrollment created via admin UI", "admin", admin.ID, "name", body.Name, "provider", body.Provider)
	writeJSON(w, http.StatusCreated, map[string]any{
		"enrollment": enrollmentResponseJSON(result.Enrollment),
		"token":      result.Token, // shown once
		"warning":    "store this token now — it will not be shown again",
	})
}

// enrollmentID gates on admin and parses the {id} path value.
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
	adminID := admin.ID
	if err := h.app.RevokeWorkerEnrollment.Execute(r.Context(), enrollment.RevokeInput{
		EnrollmentID: id,
		By:           &adminID,
		Reason:       body.Reason,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *adminAPIHandlers) prolongEnrollment(w http.ResponseWriter, r *http.Request) {
	_, id, ok := h.enrollmentID(w, r)
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
	if body.ExpiresInHours <= 0 {
		body.ExpiresInHours = 365 * 24
	}
	if body.ExpiresInHours > 365*24 {
		body.ExpiresInHours = 365 * 24 // mirror the create cap
	}
	newExpiry := time.Now().UTC().Add(time.Duration(body.ExpiresInHours) * time.Hour)
	if err := h.app.WorkerEnrollments.ExtendEnrollment(r.Context(), id, newExpiry); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": newExpiry.Format(time.RFC3339)})
}
