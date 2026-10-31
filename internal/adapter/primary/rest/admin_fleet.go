package rest

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/fleet"
)

// AdminFleetHandlers serve the worker fleet to the admin UI.
//
//	GET  /api/admin/workers
//	GET  /api/admin/worker-pools
//	POST /api/admin/worker-pools/{provider}/{name}/retire
//	GET  /api/admin/worker-enrollments                  ?provider=
//	POST /api/admin/worker-enrollments                  { name, provider, expires_in_hours?, note? }
//	POST /api/admin/worker-enrollments/{id}/revoke      { reason? }
//	POST /api/admin/worker-enrollments/{id}/prolong     { expires_in_hours? }
type AdminFleetHandlers struct {
	Auth  *Auth
	Fleet *fleet.Fleet
	// CoreRelease is this server's release, shown next to each worker's.
	CoreRelease string
	Log         *slog.Logger
}

// Mount registers the routes.
func (h *AdminFleetHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/workers", h.listWorkers)
	mux.HandleFunc("GET /api/admin/worker-pools", h.listPools)
	mux.HandleFunc("POST /api/admin/worker-pools/{provider}/{name}/retire", h.retirePool)
	mux.HandleFunc("GET /api/admin/worker-enrollments", h.listEnrollments)
	mux.HandleFunc("POST /api/admin/worker-enrollments", h.createEnrollment)
	mux.HandleFunc("POST /api/admin/worker-enrollments/{id}/revoke", h.revokeEnrollment)
	// Prolong works even after the enrollment lapsed.
	mux.HandleFunc("POST /api/admin/worker-enrollments/{id}/prolong", h.prolongEnrollment)
}

func (h *AdminFleetHandlers) listWorkers(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminGate(h.Auth, w, r); !ok {
		return
	}
	out := []map[string]any{}
	for _, hb := range h.Fleet.Workers(r.Context()) {
		out = append(out, h.workerJSON(hb))
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": out})
}

func (h *AdminFleetHandlers) workerJSON(hb domain.WorkerHeartbeat) map[string]any {
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
		"release": hb.Release, "release_compatible": hb.Compatible, "release_known": hb.Known, "core_release": h.CoreRelease,
		"provider": hb.Provider, "package": hb.Package, "go_version": hb.GoVersion, "commit": hb.Commit, "build_date": hb.BuildDate,
		"webhooks": hb.Webhooks, "capabilities": caps, "last_seen": lastSeen,
	}
}

func (h *AdminFleetHandlers) listPools(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminGate(h.Auth, w, r); !ok {
		return
	}
	pools, err := h.Fleet.PoolStatuses(r.Context())
	if err != nil {
		writeActionError(w, err, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(pools))
	for _, p := range pools {
		var retired any
		if p.RetiredAt != nil {
			retired = p.RetiredAt.UTC()
		}
		out = append(out, map[string]any{
			"provider": p.Provider, "name": p.Name, "package": p.Package,
			"first_seen_at": p.FirstSeenAt.UTC(), "last_seen_at": p.LastSeenAt.UTC(),
			"retired_at": retired, "online_instances": p.OnlineInstances,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": out})
}

func (h *AdminFleetHandlers) retirePool(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok {
		return
	}
	provider, name := r.PathValue("provider"), r.PathValue("name")
	if err := h.Fleet.RetirePool(r.Context(), actor(r, admin), provider, name); err != nil {
		writeActionError(w, err, "retire failed")
		return
	}
	h.Log.Info("worker pool retired", "provider", provider, "pool", name)
	writeJSON(w, http.StatusOK, map[string]any{"retired": true})
}

func (h *AdminFleetHandlers) listEnrollments(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminGate(h.Auth, w, r); !ok {
		return
	}
	list, err := h.Fleet.ListEnrollments(r.Context(), r.URL.Query().Get("provider"), true, true)
	if err != nil {
		writeActionError(w, err, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, adminEnrollmentJSON(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"enrollments": out, "providers": h.Fleet.Providers})
}

func (h *AdminFleetHandlers) createEnrollment(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok {
		return
	}
	var body struct {
		Name           string `json:"name"`
		Provider       string `json:"provider"`
		ExpiresInHours int    `json:"expires_in_hours"`
		Note           string `json:"note"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	e, token, err := h.Fleet.CreateEnrollment(r.Context(), actor(r, admin), fleet.EnrollmentInput{
		Name: body.Name, Provider: body.Provider, ExpiresIn: time.Duration(body.ExpiresInHours) * time.Hour, Note: body.Note,
	})
	if err != nil {
		writeActionError(w, err, "create enrollment failed")
		return
	}
	h.Log.Info("worker enrollment created via admin UI", "admin", admin.ID, "name", body.Name, "provider", body.Provider)
	writeJSON(w, http.StatusCreated, map[string]any{
		"enrollment": adminEnrollmentJSON(e),
		"token":      token, // shown once
		"warning":    "store this token now — it will not be shown again",
	})
}

func (h *AdminFleetHandlers) enrollmentID(w http.ResponseWriter, r *http.Request) (domain.User, domain.WorkerEnrollmentID, bool) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok {
		return domain.User{}, domain.WorkerEnrollmentID{}, false
	}
	id, ok := adminParseID[domain.WorkerEnrollmentID](w, r)
	return admin, id, ok
}

func (h *AdminFleetHandlers) revokeEnrollment(w http.ResponseWriter, r *http.Request) {
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
	if err := h.Fleet.RevokeEnrollment(r.Context(), actor(r, admin), id, body.Reason); err != nil {
		writeActionError(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *AdminFleetHandlers) prolongEnrollment(w http.ResponseWriter, r *http.Request) {
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
	expires, err := h.Fleet.ProlongEnrollment(r.Context(), actor(r, admin), id, time.Duration(body.ExpiresInHours)*time.Hour)
	if err != nil {
		writeActionError(w, err, "prolong failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": expires.Format(time.RFC3339)})
}

// adminEnrollmentJSON shapes a WorkerEnrollment for the enrollment responses.
func adminEnrollmentJSON(e domain.WorkerEnrollment) map[string]any {
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
