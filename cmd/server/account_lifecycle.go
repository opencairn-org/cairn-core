package main

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

// Account lifecycle — REST twins of DeleteExternalAccount and
// RotateWebhookToken. Mounted without NATS, unlike the sync routes.
//
//	DELETE /api/accounts/{id}?delete_exclusive_activities=true   → { ok, detached_sources, deleted_activities }
//	POST   /api/accounts/{id}/webhook-token/rotate                → { ok }
func mountAccountLifecycle(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.ExternalAccounts == nil {
		return
	}
	h := accountLifecycleAPI{syncHandlers{app: app, logger: logger}}
	mux.HandleFunc("DELETE /api/accounts/{id}", h.remove)
	mux.HandleFunc("POST /api/accounts/{id}/webhook-token/rotate", h.rotateToken)
	logger.Info("account lifecycle endpoints mounted")
}

type accountLifecycleAPI struct{ syncHandlers }

// userActor stamps a self-service action with the caller and request origin.
func userActor(r *http.Request, userID domain.UserID) audit.Actor {
	return audit.Actor{UserID: &userID, IP: hostOnly(clientIP(r)), UserAgent: truncateUA(r.UserAgent())}
}

func (h accountLifecycleAPI) remove(w http.ResponseWriter, r *http.Request) {
	a, ok := h.ownAccount(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if h.app.AccountRemover == nil {
		http.Error(w, "account removal not wired", http.StatusServiceUnavailable)
		return
	}
	exclusive := r.URL.Query().Get("delete_exclusive_activities") == "true"
	res, err := h.app.AccountRemover.Remove(r.Context(), userActor(r, a.UserID), a, exclusive)
	if err != nil {
		writeActionError(w, err, "remove failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detached_sources": res.DetachedSources, "deleted_activities": res.DeletedActivities})
}

func (h accountLifecycleAPI) rotateToken(w http.ResponseWriter, r *http.Request) {
	a, ok := h.ownAccount(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if h.app.WebhookTokens == nil {
		http.Error(w, "webhook tokens not wired", http.StatusServiceUnavailable)
		return
	}
	if err := h.app.WebhookTokens.Rotate(r.Context(), userActor(r, a.UserID), a.ID); err != nil {
		writeActionError(w, err, "rotate failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
