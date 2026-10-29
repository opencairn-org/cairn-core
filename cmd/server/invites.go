package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/invite"
)

// invitesAPI holds the dependencies of the invite endpoints.
type invitesAPI struct {
	app        *App
	logger     *slog.Logger
	publicURL  string
	sessionTTL time.Duration
}

// mountInvites wires the signup-invite endpoints:
//
//	POST   /api/admin/invites          (admin) → mint a code (shown once)
//	GET    /api/admin/invites          (admin) → list invites
//	POST   /api/admin/invites/{id}/revoke (admin)
//	POST   /auth/invite/redeem                → create a user from a code + log in
func mountInvites(mux *http.ServeMux, app *App, logger *slog.Logger, publicURL string, sessionTTL time.Duration) {
	if app.CreateInvite == nil || app.RedeemInvite == nil || app.Invites == nil || app.InviteAdmin == nil {
		return
	}
	h := &invitesAPI{app: app, logger: logger, publicURL: publicURL, sessionTTL: sessionTTL}
	mux.HandleFunc("POST /api/admin/invites", h.create)
	mux.HandleFunc("GET /api/admin/invites", h.list)
	mux.HandleFunc("POST /api/admin/invites/{id}/revoke", h.revoke)
	mux.HandleFunc("POST /auth/invite/redeem", h.redeem)

	logger.Info("invite endpoints mounted")
}

// --- admin: create ----------------------------------------------------
func (h *invitesAPI) create(w http.ResponseWriter, r *http.Request) {
	adminUser, ok := resolveAdminUser(r, h.app)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Role          string `json:"role"`
		Email         string `json:"email"`
		ExpiresInDays int    `json:"expires_in_days"`
		SendEmail     bool   `json:"send_email"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	role := domain.UserRole(body.Role)
	if role == "" {
		role = domain.UserRoleUser
	}
	var expiresAt *time.Time
	if body.ExpiresInDays > 0 {
		t := time.Now().UTC().Add(time.Duration(body.ExpiresInDays) * 24 * time.Hour)
		expiresAt = &t
	}
	res, err := h.app.InviteAdmin.Mint(r.Context(), restActor(r, adminUser), body.Email, role, expiresAt, body.SendEmail)
	if err != nil {
		writeActionError(w, err, "create failed")
		return
	}
	out := map[string]any{
		"id": res.Invite.ID.String(), "code": res.Code, "prefix": res.Invite.CodePrefix,
		"expires_at": fmtTimePtr(res.Invite.ExpiresAt), "delivered": res.Delivered,
		"signup_url": strings.TrimRight(h.publicURL, "/") + "/signup?code=" + res.Code,
	}
	if res.Err != nil {
		out["email_error"] = res.Err.Error()
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *invitesAPI) list(w http.ResponseWriter, r *http.Request) {
	if _, ok := resolveAdminUser(r, h.app); !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	invs, err := h.app.InviteAdmin.List(r.Context(), q.Get("include_used") != "false", q.Get("include_expired") != "false")
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	now := time.Now()
	out := make([]map[string]any, 0, len(invs))
	for _, i := range invs {
		out = append(out, map[string]any{
			"id": i.ID.String(), "prefix": i.CodePrefix, "email": i.Email,
			"role": string(i.AssignedRole), "status": i.Status(now),
			"created_at": i.CreatedAt.UTC().Format(time.RFC3339),
			"expires_at": fmtTimePtr(i.ExpiresAt),
			"used_at":    fmtTimePtr(i.UsedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": out})
}

// --- admin: revoke ----------------------------------------------------
func (h *invitesAPI) revoke(w http.ResponseWriter, r *http.Request) {
	if _, ok := resolveAdminUser(r, h.app); !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	admin, _ := resolveAdminUser(r, h.app)
	if err := h.app.InviteAdmin.Revoke(r.Context(), restActor(r, admin), domain.InviteID(id)); err != nil {
		writeActionError(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- public: redeem ---------------------------------------------------
func (h *invitesAPI) redeem(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Code        string `json:"code"`
		Username    string `json:"username"`
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	user, err := h.app.RedeemInvite.Execute(r.Context(), invite.RedeemInviteInput{
		Code:        strings.TrimSpace(body.Code),
		Username:    body.Username,
		Email:       body.Email,
		Password:    body.Password,
		DisplayName: body.DisplayName,
	})
	if err != nil {
		if errors.Is(err, domain.ErrInviteInvalid) {
			http.Error(w, "invalid or already-used invite code", http.StatusBadRequest)
			return
		}
		// User-shape / uniqueness errors are safe to surface verbatim.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Log the new user straight in.
	if err := issueSessionForUser(w, r, h.app, user.ID, domain.SessionAuthPassword, h.sessionTTL); err != nil {
		h.logger.Error("invite redeem: issue session failed", "error", err)
		http.Error(w, "account created but session failed; please sign in", http.StatusInternalServerError)
		return
	}
	// Kick off email verification for self-entered (not invite-pinned) emails.
	if !user.EmailVerified && h.app.Email != nil && user.Email != "" {
		if err := sendVerificationEmail(r.Context(), h.app, h.logger, strings.TrimRight(h.publicURL, "/"), user); err != nil {
			h.logger.Warn("invite redeem: verification email send failed", "error", err)
		}
	}
	h.logger.Info("invite redeemed", "user_id", user.ID, "username", user.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email_verified": user.EmailVerified})
}

func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
