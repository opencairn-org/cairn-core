package rest

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/usecase/federation"
	"github.com/opencairn-org/cairn-core/internal/usecase/moderation"
)

// FederationHandlers are the session-gated JSON routes of federation.
//
//	PUT    /api/profile/federation                  {enabled}: per-user opt-in
//	POST   /api/federation/follow                   {handle}: follow a remote actor
//	GET    /api/admin/federation/blocks             admin: defederated domains
//	POST   /api/admin/federation/blocks             admin: {domain, reason}
//	DELETE /api/admin/federation/blocks/{domain}    admin: lift a block
type FederationHandlers struct {
	Auth   *Auth
	Actors *federation.Actors
	Blocks *moderation.Blocks
	Log    *slog.Logger
}

// Mount registers the routes.
func (h *FederationHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("PUT /api/profile/federation", h.setOptIn)
	mux.HandleFunc("POST /api/federation/follow", h.followRemote)
	mux.HandleFunc("GET /api/admin/federation/blocks", h.listBlocks)
	mux.HandleFunc("POST /api/admin/federation/blocks", h.addBlock)
	mux.HandleFunc("DELETE /api/admin/federation/blocks/{domain}", h.removeBlock)
}

func (h *FederationHandlers) setOptIn(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.Actors.SetOptIn(r.Context(), uid, body.Enabled); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": body.Enabled})
}

func (h *FederationHandlers) followRemote(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Handle string `json:"handle"`
	}
	if err := decodeJSONBody(r, &body); err != nil || strings.TrimSpace(body.Handle) == "" {
		http.Error(w, "handle required", http.StatusBadRequest)
		return
	}
	actorURL, err := h.Actors.FollowRemote(r.Context(), uid, body.Handle)
	switch {
	case errors.Is(err, federation.ErrNotOptedIn):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, federation.ErrUnresolvable):
		http.Error(w, "could not resolve "+body.Handle, http.StatusNotFound)
	case errors.Is(err, federation.ErrRemoteUnreachable):
		http.Error(w, err.Error(), http.StatusBadGateway)
	case err != nil:
		http.Error(w, "user error", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"following": actorURL, "pending": true})
	}
}

func (h *FederationHandlers) listBlocks(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.Auth.Admin(r); !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	list, err := h.Blocks.List(r.Context())
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, b := range list {
		out = append(out, map[string]any{"domain": b.Domain, "reason": b.Reason, "created_at": b.CreatedAt.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": out})
}

func (h *FederationHandlers) addBlock(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.Auth.Admin(r)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Domain string `json:"domain"`
		Reason string `json:"reason"`
	}
	if err := decodeJSONBody(r, &body); err != nil || strings.TrimSpace(body.Domain) == "" {
		http.Error(w, "domain required", http.StatusBadRequest)
		return
	}
	d, err := h.Blocks.Block(r.Context(), actor(r, admin), body.Domain, body.Reason)
	if err != nil {
		writeActionError(w, err, "block failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"domain": d, "blocked": true})
}

func (h *FederationHandlers) removeBlock(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.Auth.Admin(r)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if err := h.Blocks.Unblock(r.Context(), actor(r, admin), r.PathValue("domain")); err != nil {
		writeActionError(w, err, "unblock failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
