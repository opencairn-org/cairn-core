package main

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// moderationAPI holds the dependencies of the moderation handlers.
type moderationAPI struct {
	app *App
}

// mountModeration wires user blocking + content reporting + the admin
// moderation queue (multi-user v1).
//
//	POST   /api/users/{id}/block      block a user (severs follows both ways)
//	DELETE /api/users/{id}/block      unblock
//	GET    /api/blocks                list the users you've blocked
//	POST   /api/reports               file a content report
//	GET    /api/admin/reports         admin: moderation queue
//	POST   /api/admin/reports/{id}/resolve  admin: mark reviewed/dismissed
//	POST   /api/admin/activities/{id}/hide  admin: hide/unhide an activity
func mountModeration(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Blocks == nil {
		return
	}
	h := &moderationAPI{app: app}
	mux.HandleFunc("POST /api/users/{id}/block", h.setBlocked(true))
	mux.HandleFunc("DELETE /api/users/{id}/block", h.setBlocked(false))
	mux.HandleFunc("GET /api/blocks", h.listBlocks)

	if app.Reports != nil {
		mux.HandleFunc("POST /api/reports", h.createReport)
		if app.ModReports != nil {
			mountModerationAdmin(mux, app)
		}
	}

	logger.Info("moderation (block + report) endpoints mounted")
}

// setBlocked blocks (true) or unblocks (false) the target user.
func (h *moderationAPI) setBlocked(block bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, ok := resolveSessionUser(r, h.app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		target, err := domain.ParseUUID[domain.UserID](r.PathValue("id"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		if block {
			err = h.app.Blocks.Block(r.Context(), me, target)
		} else {
			err = h.app.Blocks.Unblock(r.Context(), me, target)
		}
		if err != nil {
			msg := "unblock failed"
			if block {
				msg = "block failed"
			}
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"blocked": block})
	}
}

func (h *moderationAPI) listBlocks(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	ids, err := h.app.Blocks.ListBlocked(r.Context(), me)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		if u, err := h.app.Users.GetUser(r.Context(), id); err == nil {
			out = append(out, userCardJSON(u))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (h *moderationAPI) createReport(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
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
	tid, err := uuid.Parse(body.TargetID)
	if err != nil {
		http.Error(w, "bad target_id", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if len(reason) > domain.MaxReportReasonLength {
		reason = reason[:domain.MaxReportReasonLength]
	}
	id, err := h.app.Reports.Create(r.Context(), domain.ContentReport{
		ReporterID: me, TargetKind: kind, TargetID: tid, Reason: reason,
	})
	if err != nil {
		http.Error(w, "report failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id.String()})
}

// mountModerationAdmin wires the admin-only moderation queue. Gated via
// resolveAdminUser (session + admin role).
func mountModerationAdmin(mux *http.ServeMux, app *App) {
	h := &moderationAPI{app: app}
	mux.HandleFunc("GET /api/admin/reports", h.listReports)
	mux.HandleFunc("POST /api/admin/reports/{id}/resolve", h.resolveReport)
	// Role management is an admin action (delegated administration: an admin
	// promotes a trusted user to 'moderator' for the report queue).
	mux.HandleFunc("PUT /api/admin/users/{id}/role", h.setRole)
	if app.Moderation != nil {
		mux.HandleFunc("POST /api/admin/activities/{id}/hide", h.hideActivity)
	}
}

func (h *moderationAPI) listReports(w http.ResponseWriter, r *http.Request) {
	if _, ok := resolveModeratorUser(r, h.app); !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	rows, err := h.app.ModReports.List(r.Context(), domain.ReportStatus(r.URL.Query().Get("status")),
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

func (h *moderationAPI) resolveReport(w http.ResponseWriter, r *http.Request) {
	admin, ok := resolveModeratorUser(r, h.app)
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
	status, err := h.app.ModReports.Resolve(r.Context(), restActor(r, admin), id, domain.ReportStatus(body.Status))
	if err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": string(status)})
}

func (h *moderationAPI) setRole(w http.ResponseWriter, r *http.Request) {
	if _, ok := resolveAdminUser(r, h.app); !ok {
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
	admin, _ := resolveAdminUser(r, h.app)
	if h.app.UserAdmin == nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := h.app.UserAdmin.ChangeRole(r.Context(), restActor(r, admin), target, role); err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"role": string(role)})
}

func (h *moderationAPI) hideActivity(w http.ResponseWriter, r *http.Request) {
	admin, ok := resolveModeratorUser(r, h.app)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Hidden bool `json:"hidden"`
	}
	_ = decodeJSONBody(r, &body)
	if err := h.app.ModReports.SetActivityHidden(r.Context(), restActor(r, admin), id, body.Hidden); err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hidden": body.Hidden})
}
