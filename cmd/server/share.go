package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// mountShareLinks wires activity share links (multi-user v1).
//
//	POST   /api/activities/{id}/share   owner: mint an unguessable link
//	GET    /api/activities/{id}/shares  owner: list links
//	DELETE /api/shares/{token}          owner: revoke
//	GET    /api/shared/{token}          public: read-only projected activity
func mountShareLinks(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.ShareLinks == nil {
		return
	}
	h := &shareLinksAPI{app: app}
	mux.HandleFunc("POST /api/activities/{id}/share", h.create)
	mux.HandleFunc("GET /api/activities/{id}/shares", h.list)
	mux.HandleFunc("DELETE /api/shares/{token}", h.revoke)
	// Public, session-less read of a shared activity, projected via the 'link'
	// audience (which the owner's policy may further restrict).
	mux.HandleFunc("GET /api/shared/{token}", h.read)

	logger.Info("share-link endpoints mounted")
}

// shareLinksAPI holds the dependencies of the share-link handlers.
type shareLinksAPI struct {
	app *App
}

// ownedActivity resolves the caller and their {id} activity or writes an error.
func (h *shareLinksAPI) ownedActivity(w http.ResponseWriter, r *http.Request) (domain.UserID, domain.ActivityID, bool) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return me, domain.ActivityID{}, false
	}
	actID, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return me, actID, false
	}
	a, err := h.app.Activities.GetActivity(r.Context(), actID)
	if err != nil || a.UserID != me {
		http.Error(w, "not found", http.StatusNotFound)
		return me, actID, false
	}
	return me, actID, true
}

func (h *shareLinksAPI) create(w http.ResponseWriter, r *http.Request) {
	me, actID, ok := h.ownedActivity(w, r)
	if !ok {
		return
	}
	token := newShareToken()
	if err := h.app.ShareLinks.Create(r.Context(), domain.ShareLink{
		Token: token, ActivityID: actID, CreatedBy: me,
	}); err != nil {
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token,
		"path":  "/shared/" + token,
	})
}

func (h *shareLinksAPI) list(w http.ResponseWriter, r *http.Request) {
	_, actID, ok := h.ownedActivity(w, r)
	if !ok {
		return
	}
	links, err := h.app.ShareLinks.ListForActivity(r.Context(), actID)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(links))
	for _, l := range links {
		out = append(out, map[string]any{
			"token":      l.Token,
			"path":       "/shared/" + l.Token,
			"created_at": l.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			"active":     l.Active(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}

func (h *shareLinksAPI) revoke(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := h.app.ShareLinks.Revoke(r.Context(), r.PathValue("token"), me); err != nil {
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *shareLinksAPI) read(w http.ResponseWriter, r *http.Request) {
	var viewerPtr *domain.UserID
	if v, ok := optionalSessionUser(r, h.app); ok {
		viewerPtr = &v
	}
	p, owner, err := h.app.Feeds.Shared(r.Context(), h.app.ShareLinks, r.PathValue("token"), viewerPtr)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, "link not found or revoked", http.StatusNotFound)
			return
		}
		writeActionError(w, err, "load failed")
		return
	}
	act := projectedJSON(p)
	// The map and photo endpoints resolve the link audience from ?share=.
	for _, k := range []string{"map_image_url", "photos_url"} {
		if u, ok := act[k].(string); ok {
			act[k] = u + "?share=" + url.QueryEscape(r.PathValue("token"))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activity": act,
		"owner":    userCardJSON(owner),
		"shared":   true,
	})
}

// shareTokenFor reports whether the request's ?share= token is an active
// share link for actID.
func shareTokenFor(r *http.Request, app *App, actID domain.ActivityID) bool {
	token := r.URL.Query().Get("share")
	if token == "" || app.ShareLinks == nil {
		return false
	}
	link, err := app.ShareLinks.GetActive(r.Context(), token)
	return err == nil && link.ActivityID == actID
}

// newShareToken returns a 24-byte URL-safe random token (~32 chars).
func newShareToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
