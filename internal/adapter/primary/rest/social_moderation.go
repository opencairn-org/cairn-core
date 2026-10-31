package rest

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/moderation"
	"github.com/opencairn-org/cairn-core/internal/usecase/social"
	"github.com/opencairn-org/cairn-core/internal/usecase/useradmin"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// ModerationHandlers serve user blocking, content reports and the admin
// moderation queue (multi-user v1).
//
//	POST   /api/users/{id}/block            block a user (severs follows both ways)
//	DELETE /api/users/{id}/block            unblock
//	GET    /api/blocks                      the users you've blocked
//	POST   /api/reports                     file a content report
//	GET    /api/admin/reports               moderator: queue
//	POST   /api/admin/reports/{id}/resolve  moderator: mark reviewed/dismissed
//	PUT    /api/admin/users/{id}/role       admin: change a user's role
//	POST   /api/admin/activities/{id}/hide  moderator: hide/unhide an activity
type ModerationHandlers struct {
	Auth      *Auth
	Graph     *social.Graph
	Profiles  *view.Profiles
	Reports   *moderation.Reports
	UserAdmin *useradmin.Users
}

// Mount registers the block routes.
func (h *ModerationHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/users/{id}/block", h.setBlocked(true))
	mux.HandleFunc("DELETE /api/users/{id}/block", h.setBlocked(false))
	mux.HandleFunc("GET /api/blocks", h.listBlocks)
}

// MountReports registers report filing.
func (h *ModerationHandlers) MountReports(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/reports", h.createReport)
}

// MountAdmin registers the moderation queue and role management.
func (h *ModerationHandlers) MountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/reports", h.listReports)
	mux.HandleFunc("POST /api/admin/reports/{id}/resolve", h.resolveReport)
	// Delegated administration: an admin promotes a trusted user to moderator.
	mux.HandleFunc("PUT /api/admin/users/{id}/role", h.setRole)
}

// MountHide registers activity hiding.
func (h *ModerationHandlers) MountHide(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/admin/activities/{id}/hide", h.hideActivity)
}

// setBlocked blocks (true) or unblocks (false) the target user.
func (h *ModerationHandlers) setBlocked(block bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, ok := h.Auth.requireUser(w, r)
		if !ok {
			return
		}
		target, err := domain.ParseUUID[domain.UserID](r.PathValue("id"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		msg := "unblock failed"
		if block {
			msg = "block failed"
			err = h.Graph.Block(r.Context(), me, target)
		} else {
			err = h.Graph.Unblock(r.Context(), me, target)
		}
		if err != nil {
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"blocked": block})
	}
}

func (h *ModerationHandlers) listBlocks(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	users, err := h.Profiles.Blocked(r.Context(), me)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": userCardsJSON(users)})
}

func (h *ModerationHandlers) createReport(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	var body struct {
		TargetKind string `json:"target_kind"`
		TargetID   string `json:"target_id"`
		Reason     string `json:"reason"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	kind := domain.ReportTargetKind(body.TargetKind)
	if !kind.Valid() {
		http.Error(w, "bad target_kind", http.StatusBadRequest)
		return
	}
	// Reports target activities, comments or users, so the id stays untyped.
	tid, err := uuid.Parse(body.TargetID)
	if err != nil {
		http.Error(w, "bad target_id", http.StatusBadRequest)
		return
	}
	id, err := h.Reports.File(r.Context(), me, kind, tid, body.Reason)
	if err != nil {
		writeActionError(w, err, "report failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id.String()})
}

func (h *ModerationHandlers) listReports(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.Auth.Moderator(r); !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	rows, err := h.Reports.List(r.Context(), domain.ReportStatus(r.URL.Query().Get("status")),
		parseIntQuery(r, "limit", 50), parseIntQuery(r, "offset", 0))
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, rep := range rows {
		out = append(out, map[string]any{
			"id":          rep.ID.String(),
			"reporter":    rep.ReporterUsername,
			"target_kind": string(rep.TargetKind),
			"target_id":   rep.TargetID.String(),
			"reason":      rep.Reason,
			"status":      string(rep.Status),
			"created_at":  rep.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": out})
}

func (h *ModerationHandlers) resolveReport(w http.ResponseWriter, r *http.Request) {
	mod, ok := h.Auth.Moderator(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id, err := domain.ParseUUID[domain.ContentReportID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	_ = decodeJSONBody(r, &body)
	status, err := h.Reports.Resolve(r.Context(), actor(r, mod), id, domain.ReportStatus(body.Status))
	if err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": string(status)})
}

func (h *ModerationHandlers) setRole(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.Auth.Admin(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	target, err := domain.ParseUUID[domain.UserID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	role := domain.UserRole(body.Role)
	if !role.Valid() {
		http.Error(w, "invalid role", http.StatusBadRequest)
		return
	}
	if h.UserAdmin == nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := h.UserAdmin.ChangeRole(r.Context(), actor(r, admin), target, role); err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"role": string(role)})
}

func (h *ModerationHandlers) hideActivity(w http.ResponseWriter, r *http.Request) {
	mod, ok := h.Auth.Moderator(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Hidden bool `json:"hidden"`
	}
	_ = decodeJSONBody(r, &body)
	if err := h.Reports.SetActivityHidden(r.Context(), actor(r, mod), id.UUID(), body.Hidden); err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hidden": body.Hidden})
}
