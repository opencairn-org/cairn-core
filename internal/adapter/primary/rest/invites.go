package rest

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
	"github.com/opencairn-org/cairn-core/internal/usecase/invite"
)

// InviteHandlers serve signup invites.
//
//	POST /api/admin/invites              (admin) → mint a code (shown once)
//	GET  /api/admin/invites              (admin) → list invites
//	POST /api/admin/invites/{id}/revoke  (admin)
//	POST /auth/invite/redeem                     → create a user from a code + log in
type InviteHandlers struct {
	Auth         *Auth
	Admin        *invite.Admin
	Redeem       *invite.RedeemInvite
	Sessions     *authuc.Sessions
	Verification *account.EmailVerification
	// BaseURL is the public origin, without a trailing slash.
	BaseURL       string
	SecureCookies bool
	Log           *slog.Logger
}

// Mount registers the routes.
func (h *InviteHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/admin/invites", h.create)
	mux.HandleFunc("GET /api/admin/invites", h.list)
	mux.HandleFunc("POST /api/admin/invites/{id}/revoke", h.revoke)
	mux.HandleFunc("POST /auth/invite/redeem", h.redeem)
}

func (h *InviteHandlers) create(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.Auth.Admin(r)
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
	_ = decodeJSONBody(r, &body) // a bad body mints a default invite
	role := domain.UserRole(body.Role)
	if role == "" {
		role = domain.UserRoleUser
	}
	res, err := h.Admin.Mint(r.Context(), actor(r, admin), body.Email, role, acctExpiryInDays(body.ExpiresInDays), body.SendEmail)
	if err != nil {
		writeActionError(w, err, "create failed")
		return
	}
	out := map[string]any{
		"id": res.Invite.ID.String(), "code": res.Code, "prefix": res.Invite.CodePrefix,
		"expires_at": fmtTimePtr(res.Invite.ExpiresAt), "delivered": res.Delivered,
		"signup_url": h.BaseURL + "/signup?code=" + res.Code,
	}
	if res.Err != nil {
		out["email_error"] = res.Err.Error()
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *InviteHandlers) list(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.Auth.Admin(r); !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	invs, err := h.Admin.List(r.Context(), q.Get("include_used") != "false", q.Get("include_expired") != "false")
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

func (h *InviteHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.Auth.Admin(r)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	id, err := domain.ParseUUID[domain.InviteID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.Admin.Revoke(r.Context(), actor(r, admin), id); err != nil {
		writeActionError(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *InviteHandlers) redeem(w http.ResponseWriter, r *http.Request) {
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
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	user, err := h.Redeem.Execute(r.Context(), invite.RedeemInviteInput{
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
	if err := acctIssueSession(r.Context(), w, r, h.Sessions, h.SecureCookies, user.ID, domain.SessionAuthPassword); err != nil {
		h.Log.Error("invite redeem: issue session failed", "error", err)
		http.Error(w, "account created but session failed; please sign in", http.StatusInternalServerError)
		return
	}
	// Self-entered (not invite-pinned) addresses still need verifying.
	if !user.EmailVerified && h.Verification != nil && h.Verification.Mail != nil && user.Email != "" {
		if err := h.Verification.Send(r.Context(), user); err != nil {
			h.Log.Warn("invite redeem: verification email send failed", "error", err)
		}
	}
	h.Log.Info("invite redeemed", "user_id", user.ID, "username", user.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email_verified": user.EmailVerified})
}
