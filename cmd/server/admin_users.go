package main

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
	"github.com/opencairn-org/cairn-core/internal/usecase/useradmin"
)

// mountAdminUsers is the REST twin of the AdminService user and audit RPCs:
//
//	GET    /api/admin/users                      ?cursor=&limit=
//	POST   /api/admin/users                      { username, email, display_name, role, initial_password?, locale, timezone, units }
//	GET    /api/admin/users/{id}
//	PUT    /api/admin/users/{id}                 { email?, display_name?, locale?, timezone?, units? }
//	PUT    /api/admin/users/{id}/role            { role }            (also mounted by moderation)
//	PUT    /api/admin/users/{id}/status          { status, reason }
//	DELETE /api/admin/users/{id}                 ?confirm_username=&hard=
//	POST   /api/admin/users/{id}/reset-password
//	POST   /api/admin/users/{id}/revoke-credentials { sessions, pats, passkeys }
//	GET    /api/admin/audit                      ?from=&to=&actor=&resource_type=&resource_id=&action=&min_severity=&q=&limit=&offset=
func mountAdminUsers(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.UserAdmin == nil {
		return
	}
	h := &adminUserHandlers{app: app, logger: logger}
	mux.HandleFunc("GET /api/admin/users", h.list)
	mux.HandleFunc("POST /api/admin/users", h.create)
	mux.HandleFunc("GET /api/admin/users/{id}", h.get)
	mux.HandleFunc("PUT /api/admin/users/{id}", h.update)
	mux.HandleFunc("PUT /api/admin/users/{id}/status", h.setStatus)
	mux.HandleFunc("DELETE /api/admin/users/{id}", h.del)
	mux.HandleFunc("POST /api/admin/users/{id}/reset-password", h.resetPassword)
	mux.HandleFunc("POST /api/admin/users/{id}/revoke-credentials", h.revokeCredentials)
	mux.HandleFunc("GET /api/admin/audit", h.audit)
}

type adminUserHandlers struct {
	app    *App
	logger *slog.Logger
}

// restActor builds the audit actor of an admin request.
func restActor(r *http.Request, admin domain.User) audit.Actor {
	a := audit.UserActor(admin)
	a.IP = hostOnly(clientIP(r))
	a.UserAgent = truncateUA(r.UserAgent())
	return a
}

func (h *adminUserHandlers) gate(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	admin, ok := resolveAdminUser(r, h.app)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
	}
	return admin, ok
}

func (h *adminUserHandlers) target(w http.ResponseWriter, r *http.Request) (domain.UserID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return domain.UserID{}, false
	}
	return domain.UserID(id), true
}

func (h *adminUserHandlers) list(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.gate(w, r); !ok {
		return
	}
	rows, next, err := h.app.UserAdmin.Page(r.Context(), r.URL.Query().Get("cursor"), parseIntQuery(r, "limit", 50))
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, u := range rows {
		fp := u.Footprint
		row := userJSON(u.User)
		row["activity_count"], row["external_account_count"], row["storage_bytes_used"] = fp.Activities, fp.ExternalAccounts, fp.StorageBytes
		row["last_activity_at"] = fmtTimePtr(fp.LastActivityAt)
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "next_cursor": next})
}

func (h *adminUserHandlers) get(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.gate(w, r); !ok {
		return
	}
	id, ok := h.target(w, r)
	if !ok {
		return
	}
	d, err := h.app.UserAdmin.Get(r.Context(), id)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	idents := make([]map[string]any, 0, len(d.Identities))
	for _, li := range d.Identities {
		idents = append(idents, map[string]any{"id": li.ID.String(), "provider": li.Provider, "subject": li.Subject, "email": li.Email,
			"linked_at": li.LinkedAt.UTC().Format(time.RFC3339), "last_used_at": fmtTimePtr(li.LastUsedAt)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": userJSON(d.User), "identities": idents, "active_session_count": d.ActiveSessions,
		"activity_count": d.Footprint.Activities, "external_account_count": d.Footprint.ExternalAccounts,
		"storage_bytes_used": d.Footprint.StorageBytes, "last_activity_at": fmtTimePtr(d.Footprint.LastActivityAt),
	})
}

type adminUserBody struct {
	Username        string  `json:"username"`
	Email           *string `json:"email"`
	DisplayName     *string `json:"display_name"`
	Role            string  `json:"role"`
	InitialPassword string  `json:"initial_password"`
	Locale          *string `json:"locale"`
	Timezone        *string `json:"timezone"`
	Units           *string `json:"units"`
	Status          string  `json:"status"`
	Reason          string  `json:"reason"`
	Sessions        bool    `json:"sessions"`
	PATs            bool    `json:"pats"`
	Passkeys        bool    `json:"passkeys"`
}

func (h *adminUserHandlers) create(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.gate(w, r)
	if !ok || !requireJSON(w, r) {
		return
	}
	var b adminUserBody
	if err := decodeJSONBody(r, &b); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	res, err := h.app.UserAdmin.CreateUser(r.Context(), restActor(r, admin), useradmin.CreateInput{
		Username: b.Username, Email: deref(b.Email), DisplayName: deref(b.DisplayName), Role: domain.UserRole(b.Role),
		InitialPassword: b.InitialPassword, Locale: deref(b.Locale), Timezone: deref(b.Timezone), Units: domain.UserUnits(deref(b.Units)),
	})
	if err != nil {
		writeActionError(w, err, "create failed")
		return
	}
	out := map[string]any{"user": userJSON(res.User)}
	if res.Invite != nil {
		out["invite"] = map[string]any{"id": res.Invite.ID.String(), "code": res.Invite.Code, "expires_at": fmtTimePtr(res.Invite.ExpiresAt),
			"signup_url": strings.TrimRight(h.app.PublicBaseURL, "/") + "/signup?code=" + res.Invite.Code}
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *adminUserHandlers) update(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.gate(w, r)
	if !ok || !requireJSON(w, r) {
		return
	}
	id, ok := h.target(w, r)
	if !ok {
		return
	}
	var b adminUserBody
	if err := decodeJSONBody(r, &b); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	u, err := h.app.UserAdmin.Update(r.Context(), restActor(r, admin), id, useradmin.UserPatch{
		Email: b.Email, DisplayName: b.DisplayName, Locale: b.Locale, Timezone: b.Timezone, Units: b.Units,
	})
	if err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": userJSON(u)})
}

func (h *adminUserHandlers) setStatus(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.gate(w, r)
	if !ok || !requireJSON(w, r) {
		return
	}
	id, ok := h.target(w, r)
	if !ok {
		return
	}
	var b adminUserBody
	if err := decodeJSONBody(r, &b); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	u, err := h.app.UserAdmin.SetStatus(r.Context(), restActor(r, admin), id, domain.UserStatus(b.Status), b.Reason)
	if err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": userJSON(u)})
}

func (h *adminUserHandlers) del(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.gate(w, r)
	if !ok {
		return
	}
	id, ok := h.target(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	counts, err := h.app.UserAdmin.Delete(r.Context(), restActor(r, admin), id, q.Get("confirm_username"), q.Get("hard") == "true")
	if err != nil {
		writeActionError(w, err, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"activities_deleted": counts.Activities, "external_accounts_deleted": counts.ExternalAccounts, "storage_bytes_freed": counts.StorageBytes})
}

func (h *adminUserHandlers) resetPassword(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.gate(w, r)
	if !ok {
		return
	}
	id, ok := h.target(w, r)
	if !ok {
		return
	}
	temp, err := h.app.UserAdmin.ResetPassword(r.Context(), restActor(r, admin), id)
	if err != nil {
		writeActionError(w, err, "reset failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"temporary_password": temp})
}

func (h *adminUserHandlers) revokeCredentials(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.gate(w, r)
	if !ok {
		return
	}
	id, ok := h.target(w, r)
	if !ok {
		return
	}
	var b adminUserBody
	_ = decodeOptionalJSONBody(r, &b)
	res, err := h.app.UserAdmin.RevokeCredentials(r.Context(), restActor(r, admin), id, b.Sessions, b.PATs, b.Passkeys)
	if err != nil {
		writeActionError(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions_revoked": res.Sessions, "pats_revoked": res.PATs, "passkeys_revoked": res.Passkeys})
}

func (h *adminUserHandlers) audit(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.gate(w, r); !ok {
		return
	}
	q := r.URL.Query()
	f := domain.AuditFilter{
		ResourceType: q.Get("resource_type"), ResourceID: q.Get("resource_id"), Query: q.Get("q"),
		MinSeverity: domain.AuditSeverity(q.Get("min_severity")), Limit: parseIntQuery(r, "limit", 50), Offset: parseIntQuery(r, "offset", 0),
	}
	f.From, f.To = parseTimeQuery(q.Get("from")), parseTimeQuery(q.Get("to"))
	if a := q.Get("action"); a != "" {
		f.Actions = strings.Split(a, ",")
	}
	if raw := q.Get("actor"); raw != "" {
		uid, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "bad actor", http.StatusBadRequest)
			return
		}
		u := domain.UserID(uid)
		f.ActorUserID = &u
	}
	entries, err := h.app.AuditLog.List(r.Context(), f)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		diff := map[string]any{}
		for _, c := range e.Diff {
			diff[c.Field] = map[string]string{"before": c.Before, "after": c.After}
		}
		row := map[string]any{
			"id": e.ID.String(), "ts": e.At.UTC().Format(time.RFC3339), "actor_username": e.ActorUsername, "ip_address": e.IPAddress,
			"action": e.Action, "severity": string(e.Severity), "resource_type": e.ResourceType, "resource_id": e.ResourceID,
			"diff": diff, "metadata": e.Metadata,
		}
		if e.ActorUserID != nil {
			row["actor_user_id"] = e.ActorUserID.String()
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// hostOnly strips a port from an address for the audit log's inet column.
func hostOnly(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i > 0 && strings.Count(addr, ":") == 1 {
		addr = addr[:i]
	}
	return strings.Trim(addr, "[]")
}
