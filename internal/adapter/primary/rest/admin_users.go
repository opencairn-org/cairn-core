package rest

import (
	"net/http"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
	"github.com/opencairn-org/cairn-core/internal/usecase/useradmin"
)

// AdminUserHandlers are the REST twin of the AdminService user and audit RPCs.
//
//	GET    /api/admin/users                      ?cursor=&limit=
//	POST   /api/admin/users                      { username, email, display_name, role, initial_password?, locale, timezone, units }
//	GET    /api/admin/users/{id}
//	PUT    /api/admin/users/{id}                 { email?, display_name?, locale?, timezone?, units? }
//	PUT    /api/admin/users/{id}/status          { status, reason }
//	DELETE /api/admin/users/{id}                 ?confirm_username=&hard=
//	POST   /api/admin/users/{id}/reset-password
//	POST   /api/admin/users/{id}/revoke-credentials { sessions, pats, passkeys }
//	GET    /api/admin/audit                      ?from=&to=&actor=&resource_type=&resource_id=&action=&min_severity=&q=&limit=&offset=
//
// PUT /api/admin/users/{id}/role is mounted by moderation.
type AdminUserHandlers struct {
	Auth  *Auth
	Users *useradmin.Users
	Audit *audit.Recorder
	// PublicBaseURL prefixes the signup link of an invite-based account.
	PublicBaseURL string
}

// Mount registers the routes.
func (h *AdminUserHandlers) Mount(mux *http.ServeMux) {
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

// target gates on admin and parses the {id} user.
func (h *AdminUserHandlers) target(w http.ResponseWriter, r *http.Request) (domain.User, domain.UserID, bool) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok {
		return admin, domain.UserID{}, false
	}
	id, ok := adminParseID[domain.UserID](w, r)
	return admin, id, ok
}

// body gates on admin and JSON, parses {id} and decodes the body.
func (h *AdminUserHandlers) body(w http.ResponseWriter, r *http.Request) (domain.User, domain.UserID, adminUserBody, bool) {
	var b adminUserBody
	admin, ok := adminGate(h.Auth, w, r)
	if !ok || !requireJSON(w, r) {
		return admin, domain.UserID{}, b, false
	}
	id, ok := adminParseID[domain.UserID](w, r)
	if !ok {
		return admin, id, b, false
	}
	if err := decodeJSONBody(r, &b); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return admin, id, b, false
	}
	return admin, id, b, true
}

func (h *AdminUserHandlers) list(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminGate(h.Auth, w, r); !ok {
		return
	}
	rows, next, err := h.Users.Page(r.Context(), r.URL.Query().Get("cursor"), parseIntQuery(r, "limit", 50))
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, u := range rows {
		fp := u.Footprint
		row := adminUserJSON(u.User)
		row["activity_count"], row["external_account_count"], row["storage_bytes_used"] = fp.Activities, fp.ExternalAccounts, fp.StorageBytes
		row["last_activity_at"] = adminTimePtr(fp.LastActivityAt)
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "next_cursor": next})
}

func (h *AdminUserHandlers) get(w http.ResponseWriter, r *http.Request) {
	_, id, ok := h.target(w, r)
	if !ok {
		return
	}
	d, err := h.Users.Get(r.Context(), id)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	idents := make([]map[string]any, 0, len(d.Identities))
	for _, li := range d.Identities {
		idents = append(idents, map[string]any{"id": li.ID.String(), "provider": li.Provider, "subject": li.Subject, "email": li.Email,
			"linked_at": li.LinkedAt.UTC().Format(time.RFC3339), "last_used_at": adminTimePtr(li.LastUsedAt)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": adminUserJSON(d.User), "identities": idents, "active_session_count": d.ActiveSessions,
		"activity_count": d.Footprint.Activities, "external_account_count": d.Footprint.ExternalAccounts,
		"storage_bytes_used": d.Footprint.StorageBytes, "last_activity_at": adminTimePtr(d.Footprint.LastActivityAt),
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

func (h *AdminUserHandlers) create(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok || !requireJSON(w, r) {
		return
	}
	var b adminUserBody
	if err := decodeJSONBody(r, &b); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	res, err := h.Users.CreateUser(r.Context(), actor(r, admin), useradmin.CreateInput{
		Username: b.Username, Email: adminDeref(b.Email), DisplayName: adminDeref(b.DisplayName), Role: domain.UserRole(b.Role),
		InitialPassword: b.InitialPassword, Locale: adminDeref(b.Locale), Timezone: adminDeref(b.Timezone), Units: domain.UserUnits(adminDeref(b.Units)),
	})
	if err != nil {
		writeActionError(w, err, "create failed")
		return
	}
	out := map[string]any{"user": adminUserJSON(res.User)}
	if res.Invite != nil {
		out["invite"] = map[string]any{"id": res.Invite.ID.String(), "code": res.Invite.Code, "expires_at": adminTimePtr(res.Invite.ExpiresAt),
			"signup_url": strings.TrimRight(h.PublicBaseURL, "/") + "/signup?code=" + res.Invite.Code}
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *AdminUserHandlers) update(w http.ResponseWriter, r *http.Request) {
	admin, id, b, ok := h.body(w, r)
	if !ok {
		return
	}
	u, err := h.Users.Update(r.Context(), actor(r, admin), id, useradmin.UserPatch{
		Email: b.Email, DisplayName: b.DisplayName, Locale: b.Locale, Timezone: b.Timezone, Units: b.Units,
	})
	if err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": adminUserJSON(u)})
}

func (h *AdminUserHandlers) setStatus(w http.ResponseWriter, r *http.Request) {
	admin, id, b, ok := h.body(w, r)
	if !ok {
		return
	}
	u, err := h.Users.SetStatus(r.Context(), actor(r, admin), id, domain.UserStatus(b.Status), b.Reason)
	if err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": adminUserJSON(u)})
}

func (h *AdminUserHandlers) del(w http.ResponseWriter, r *http.Request) {
	admin, id, ok := h.target(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	counts, err := h.Users.Delete(r.Context(), actor(r, admin), id, q.Get("confirm_username"), q.Get("hard") == "true")
	if err != nil {
		writeActionError(w, err, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"activities_deleted": counts.Activities, "external_accounts_deleted": counts.ExternalAccounts, "storage_bytes_freed": counts.StorageBytes})
}

func (h *AdminUserHandlers) resetPassword(w http.ResponseWriter, r *http.Request) {
	admin, id, ok := h.target(w, r)
	if !ok {
		return
	}
	temp, err := h.Users.ResetPassword(r.Context(), actor(r, admin), id)
	if err != nil {
		writeActionError(w, err, "reset failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"temporary_password": temp})
}

func (h *AdminUserHandlers) revokeCredentials(w http.ResponseWriter, r *http.Request) {
	admin, id, ok := h.target(w, r)
	if !ok {
		return
	}
	var b adminUserBody
	_ = decodeOptionalJSONBody(r, &b)
	res, err := h.Users.RevokeCredentials(r.Context(), actor(r, admin), id, b.Sessions, b.PATs, b.Passkeys)
	if err != nil {
		writeActionError(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions_revoked": res.Sessions, "pats_revoked": res.PATs, "passkeys_revoked": res.Passkeys})
}

func (h *AdminUserHandlers) audit(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminGate(h.Auth, w, r); !ok {
		return
	}
	q := r.URL.Query()
	f := domain.AuditFilter{
		ResourceType: q.Get("resource_type"), ResourceID: q.Get("resource_id"), Query: q.Get("q"),
		MinSeverity: domain.AuditSeverity(q.Get("min_severity")), Limit: parseIntQuery(r, "limit", 50), Offset: parseIntQuery(r, "offset", 0),
	}
	f.From, f.To = adminParseTime(q.Get("from")), adminParseTime(q.Get("to"))
	if a := q.Get("action"); a != "" {
		f.Actions = strings.Split(a, ",")
	}
	if raw := q.Get("actor"); raw != "" {
		uid, err := domain.ParseUUID[domain.UserID](raw)
		if err != nil {
			http.Error(w, "bad actor", http.StatusBadRequest)
			return
		}
		f.ActorUserID = &uid
	}
	entries, err := h.Audit.List(r.Context(), f)
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
