package rest

import (
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/accounts"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
	"github.com/opencairn-org/cairn-core/internal/usecase/providerlink"
	"github.com/opencairn-org/cairn-core/internal/usecase/webhooks"
)

// AccountLifecycleHandlers are the REST twins of DeleteExternalAccount and
// RotateWebhookToken. Anyone else's account reads as 404.
//
//	DELETE /api/accounts/{id}?delete_exclusive_activities=true   → { ok, detached_sources, deleted_activities }
//	POST   /api/accounts/{id}/webhook-token/rotate                → { ok }
type AccountLifecycleHandlers struct {
	Auth     *Auth
	Accounts *providerlink.Accounts
	Remover  *accounts.Remover
	Tokens   *webhooks.Tokens
}

// Mount registers the routes.
func (h *AccountLifecycleHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("DELETE /api/accounts/{id}", h.remove)
	mux.HandleFunc("POST /api/accounts/{id}/webhook-token/rotate", h.rotateToken)
}

// owned is the caller's {id} account; false for anonymous callers and others' accounts.
func (h *AccountLifecycleHandlers) owned(r *http.Request) (domain.ExternalAccount, bool) {
	userID, ok := h.Auth.User(r)
	if !ok {
		return domain.ExternalAccount{}, false
	}
	id, err := domain.ParseUUID[domain.ExternalAccountID](r.PathValue("id"))
	if err != nil {
		return domain.ExternalAccount{}, false
	}
	a, err := h.Accounts.Owned(r.Context(), userID, id)
	return a, err == nil
}

// acctUserActor stamps a self-service action with the caller and request origin.
func acctUserActor(r *http.Request, userID domain.UserID) audit.Actor {
	return audit.Actor{UserID: &userID, IP: hostOnly(clientIP(r)), UserAgent: truncateUA(r.UserAgent())}
}

func (h *AccountLifecycleHandlers) remove(w http.ResponseWriter, r *http.Request) {
	a, ok := h.owned(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if h.Remover == nil {
		http.Error(w, "account removal not wired", http.StatusServiceUnavailable)
		return
	}
	exclusive := r.URL.Query().Get("delete_exclusive_activities") == "true"
	res, err := h.Remover.Remove(r.Context(), acctUserActor(r, a.UserID), a, exclusive)
	if err != nil {
		writeActionError(w, err, "remove failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detached_sources": res.DetachedSources, "deleted_activities": res.DeletedActivities})
}

func (h *AccountLifecycleHandlers) rotateToken(w http.ResponseWriter, r *http.Request) {
	a, ok := h.owned(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if h.Tokens == nil {
		http.Error(w, "webhook tokens not wired", http.StatusServiceUnavailable)
		return
	}
	if err := h.Tokens.Rotate(r.Context(), acctUserActor(r, a.UserID), a.ID); err != nil {
		writeActionError(w, err, "rotate failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
