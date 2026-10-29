package main

import (
	"crypto/sha256"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
)

// mountAccount exposes the self-service account routes that mirror the
// AuthService RPCs of the same names (shared use cases in usecase/account):
//
//	POST   /auth/password/change   { current_password, new_password }
//	GET    /api/sessions           → { sessions: [...] }   (is_current marks the caller's)
//	DELETE /api/sessions/{id}
//	GET    /api/identities         → { identities: [...] }
//	DELETE /api/identities/{id}    (refused for the last sign-in method)
//	PUT    /api/profile            { display_name?, avatar_url?, locale?, timezone?, units? } → { user }
func mountAccount(mux *http.ServeMux, app *App, logger *slog.Logger) {
	h := &accountHandlers{app: app}
	if app.Passwords != nil {
		mux.HandleFunc("POST /auth/password/change", h.changePassword)
	}
	if app.SessionMgmt != nil {
		mux.HandleFunc("GET /api/sessions", h.listSessions)
		mux.HandleFunc("DELETE /api/sessions/{id}", h.revokeSession)
	}
	if app.Identities != nil {
		mux.HandleFunc("GET /api/identities", h.listIdentities)
		mux.HandleFunc("DELETE /api/identities/{id}", h.unlinkIdentity)
	}
	if app.Profiles != nil {
		mux.HandleFunc("PUT /api/profile", h.updateProfile)
	}
	logger.Info("account endpoints mounted")
}

type accountHandlers struct{ app *App }

// currentSessionID resolves the cookie session behind the request (zero for bearer callers).
func currentSessionID(r *http.Request, app *App) domain.SessionID {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return domain.SessionID{}
	}
	hash := sha256.Sum256([]byte(c.Value))
	sess, err := app.Sessions.GetByTokenHash(r.Context(), hash[:])
	if err != nil {
		return domain.SessionID{}
	}
	return sess.ID
}

func (h *accountHandlers) changePassword(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
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
	keep := currentSessionID(r, h.app)
	if err := h.app.Passwords.Change(r.Context(), userID, keep, body.Current, body.Next); err != nil {
		writeActionError(w, err, "change failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *accountHandlers) listSessions(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	current := currentSessionID(r, h.app)
	list, err := h.app.SessionMgmt.List(r.Context(), userID)
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

func (h *accountHandlers) revokeSession(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.app.SessionMgmt.Revoke(r.Context(), userID, domain.SessionID(id)); err != nil {
		writeActionError(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *accountHandlers) listIdentities(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	list, err := h.app.Identities.List(r.Context(), userID)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, li := range list {
		out = append(out, map[string]any{
			"id":            uuid.UUID(li.ID).String(),
			"provider":      li.Provider,
			"provider_name": h.app.Identities.ProviderName(li.Provider),
			"subject":       li.Subject,
			"email":         li.Email,
			"linked_at":     li.LinkedAt.UTC().Format(time.RFC3339),
			"last_used_at":  fmtTimePtr(li.LastUsedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"identities": out})
}

func (h *accountHandlers) unlinkIdentity(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.app.Identities.Unlink(r.Context(), userID, domain.LinkedIdentityID(id)); err != nil {
		writeActionError(w, err, "unlink failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *accountHandlers) updateProfile(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
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
	u, err := h.app.Profiles.Update(r.Context(), userID, patch)
	if err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": userJSON(u)})
}

// userJSON renders the caller's own user row (the REST twin of proto User).
func userJSON(u domain.User) map[string]any {
	return map[string]any{
		"id":             u.ID.String(),
		"username":       u.Username,
		"email":          u.Email,
		"email_verified": u.EmailVerified,
		"display_name":   u.DisplayName,
		"avatar_url":     u.AvatarURL,
		"locale":         u.Locale,
		"timezone":       u.Timezone,
		"units":          string(u.Units),
		"date_format":    u.DateFormat,
		"time_format":    u.TimeFormat,
		"role":           string(u.Role),
		"status":         string(u.Status),
		"created_at":     u.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":     u.UpdatedAt.UTC().Format(time.RFC3339),
		"last_login_at":  fmtTimePtr(u.LastLoginAt),
	}
}
