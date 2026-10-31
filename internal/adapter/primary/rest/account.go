package rest

import (
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
)

// AccountHandlers serve the self-service account routes, REST twins of the
// AuthService RPCs of the same names. Nil use cases leave their routes unmounted.
//
//	POST   /auth/password/change          { current_password, new_password }
//	GET    /api/sessions                  → { sessions: [...] } (is_current marks the caller's)
//	DELETE /api/sessions/{id}
//	GET    /api/identities                → { identities: [...] }
//	DELETE /api/identities/{id}           (refused for the last sign-in method)
//	PUT    /api/profile                   { display_name?, avatar_url?, locale?, timezone?, units? } → { user }
//	GET    /api/oauth/apps                → { apps: [...] }
//	DELETE /api/oauth/apps/{client_id}    → { tokens_revoked }
//	GET    /api/pats                      → list (never returns the token)
//	POST   /api/pats                      { name, expires_in_days? } → token shown once
//	DELETE /api/pats/{id}
type AccountHandlers struct {
	Auth           *Auth
	Sessions       *authuc.Sessions
	Passwords      *account.Passwords
	SessionMgmt    *account.Sessions
	Identities     *account.Identities
	Profiles       *account.Profiles
	AuthorizedApps *account.AuthorizedApps
	Tokens         *account.Tokens
}

// Mount registers the routes.
func (h *AccountHandlers) Mount(mux *http.ServeMux) {
	if h.Passwords != nil {
		mux.HandleFunc("POST /auth/password/change", h.changePassword)
	}
	if h.SessionMgmt != nil {
		mux.HandleFunc("GET /api/sessions", h.listSessions)
		mux.HandleFunc("DELETE /api/sessions/{id}", h.revokeSession)
	}
	if h.Identities != nil {
		mux.HandleFunc("GET /api/identities", h.listIdentities)
		mux.HandleFunc("DELETE /api/identities/{id}", h.unlinkIdentity)
	}
	if h.Profiles != nil {
		mux.HandleFunc("PUT /api/profile", h.updateProfile)
	}
	if h.AuthorizedApps != nil {
		mux.HandleFunc("GET /api/oauth/apps", h.listApps)
		mux.HandleFunc("DELETE /api/oauth/apps/{client_id}", h.revokeApp)
	}
	if h.Tokens != nil {
		mux.HandleFunc("GET /api/pats", h.listPATs)
		mux.HandleFunc("POST /api/pats", h.createPAT)
		mux.HandleFunc("DELETE /api/pats/{id}", h.revokePAT)
	}
}

// currentSession is the session behind the request (zero for PAT/OAuth callers).
func (h *AccountHandlers) currentSession(r *http.Request) domain.SessionID {
	if h.Sessions == nil {
		return domain.SessionID{}
	}
	return h.Sessions.Lookup(r.Context(), acctSessionToken(r))
}

func (h *AccountHandlers) changePassword(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Current string `json:"current_password"`
		Next    string `json:"new_password"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.Passwords.Change(r.Context(), userID, h.currentSession(r), body.Current, body.Next); err != nil {
		writeActionError(w, err, "change failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *AccountHandlers) listSessions(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	current := h.currentSession(r)
	list, err := h.SessionMgmt.List(r.Context(), userID)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, s := range list {
		m := map[string]any{
			"id":                 uuid.UUID(s.ID).String(),
			"auth_method":        string(s.AuthMethod),
			"user_agent_summary": s.UserAgentSummary,
			"ip_geo_summary":     s.IPGeoSummary,
			"created_at":         s.CreatedAt.UTC().Format(time.RFC3339),
			"last_seen_at":       s.LastSeenAt.UTC().Format(time.RFC3339),
			"expires_at":         s.ExpiresAt.UTC().Format(time.RFC3339),
			"is_current":         s.ID == current,
		}
		if s.IPAddress.IsValid() {
			m["ip_address"] = s.IPAddress.String()
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (h *AccountHandlers) revokeSession(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.SessionID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.SessionMgmt.Revoke(r.Context(), userID, id); err != nil {
		writeActionError(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *AccountHandlers) listIdentities(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	list, err := h.Identities.List(r.Context(), userID)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, li := range list {
		out = append(out, map[string]any{
			"id":            uuid.UUID(li.ID).String(),
			"provider":      li.Provider,
			"provider_name": h.Identities.ProviderName(li.Provider),
			"subject":       li.Subject,
			"email":         li.Email,
			"linked_at":     li.LinkedAt.UTC().Format(time.RFC3339),
			"last_used_at":  acctFmtTimePtr(li.LastUsedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"identities": out})
}

func (h *AccountHandlers) unlinkIdentity(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.LinkedIdentityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.Identities.Unlink(r.Context(), userID, id); err != nil {
		writeActionError(w, err, "unlink failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *AccountHandlers) updateProfile(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body map[string]*string
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if _, has := body["email"]; has {
		http.Error(w, account.ErrEmailChangeUnsupported.Error(), http.StatusBadRequest)
		return
	}
	known := []string{"display_name", "avatar_url", "locale", "timezone", "units"}
	for k := range body {
		if !slices.Contains(known, k) {
			http.Error(w, "unknown field "+k, http.StatusBadRequest)
			return
		}
	}
	patch := account.ProfilePatch{
		DisplayName: body["display_name"], AvatarURL: body["avatar_url"],
		Locale: body["locale"], Timezone: body["timezone"], Units: body["units"],
	}
	u, err := h.Profiles.Update(r.Context(), userID, patch)
	if err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": acctUserJSON(u)})
}

// acctUserJSON renders the caller's own user row (the REST twin of proto User).
func acctUserJSON(u domain.User) map[string]any {
	return map[string]any{
		"id":                    u.ID.String(),
		"username":              u.Username,
		"email":                 u.Email,
		"email_verified":        u.EmailVerified,
		"display_name":          u.DisplayName,
		"avatar_url":            u.AvatarURL,
		"locale":                u.Locale,
		"timezone":              u.Timezone,
		"units":                 string(u.Units),
		"date_format":           u.DateFormat,
		"time_format":           u.TimeFormat,
		"role":                  string(u.Role),
		"status":                string(u.Status),
		"passkey_second_factor": u.PasskeySecondFactor,
		"created_at":            u.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":            u.UpdatedAt.UTC().Format(time.RFC3339),
		"last_login_at":         acctFmtTimePtr(u.LastLoginAt),
	}
}

func (h *AccountHandlers) listApps(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	grants, err := h.AuthorizedApps.List(r.Context(), userID)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(grants))
	for _, g := range grants {
		out = append(out, map[string]any{
			"client_id": g.ClientID, "client_name": g.ClientName, "software_id": g.SoftwareID,
			"software_version": g.SoftwareVersion, "scopes": g.Scopes, "active_tokens": g.ActiveTokens,
			"granted_at": g.GrantedAt.UTC().Format(time.RFC3339), "last_issued_at": g.LastIssuedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

func (h *AccountHandlers) revokeApp(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	n, err := h.AuthorizedApps.Revoke(r.Context(), userID, r.PathValue("client_id"))
	if err != nil {
		writeActionError(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens_revoked": n})
}

func (h *AccountHandlers) listPATs(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	list, err := h.Tokens.List(r.Context(), userID)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	now := time.Now()
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		status := "active"
		if p.RevokedAt != nil {
			status = "revoked"
		} else if !p.IsValidAt(now) {
			status = "expired"
		}
		out = append(out, map[string]any{
			"id": p.ID.String(), "name": p.Name, "prefix": p.TokenPrefix, "status": status,
			"created_at":   p.CreatedAt.UTC().Format(time.RFC3339),
			"expires_at":   acctFmtTimePtr(p.ExpiresAt),
			"last_used_at": acctFmtTimePtr(p.LastUsedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (h *AccountHandlers) createPAT(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	_ = decodeJSONBody(r, &body) // a bad body leaves the name empty, which Create refuses
	pat, token, err := h.Tokens.Create(r.Context(), userID, body.Name, nil, acctExpiryInDays(body.ExpiresInDays))
	if err != nil {
		writeActionError(w, err, "create failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": pat.ID.String(), "token": token})
}

// acctExpiryInDays is now+days, nil (never) for days <= 0.
func acctExpiryInDays(days int) *time.Time {
	if days <= 0 {
		return nil
	}
	t := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
	return &t
}

func (h *AccountHandlers) revokePAT(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.PATID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.Tokens.Revoke(r.Context(), userID, id); err != nil {
		writeActionError(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
