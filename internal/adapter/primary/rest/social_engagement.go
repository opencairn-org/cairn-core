package rest

import (
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/social"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// EngagementHandlers serve kudos + comments (multi-user v1). Reads and writes
// need CategorySocial on the activity (or ownership).
//
//	GET    /api/activities/{id}/kudos
//	POST   /api/activities/{id}/kudos
//	DELETE /api/activities/{id}/kudos
//	GET    /api/activities/{id}/comments
//	POST   /api/activities/{id}/comments
//	DELETE /api/comments/{id}
type EngagementHandlers struct {
	Auth       *Auth
	Engagement *social.Engagement
	Details    *view.Details
}

// Mount registers the routes.
func (h *EngagementHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/activities/{id}/kudos", h.listKudos)
	mux.HandleFunc("POST /api/activities/{id}/kudos", h.addKudos)
	mux.HandleFunc("DELETE /api/activities/{id}/kudos", h.removeKudos)
	mux.HandleFunc("GET /api/activities/{id}/comments", h.listComments)
	mux.HandleFunc("POST /api/activities/{id}/comments", h.addComment)
	mux.HandleFunc("DELETE /api/comments/{id}", h.deleteComment)
}

// gate resolves the caller and an activity they may react to, or writes an error.
func (h *EngagementHandlers) gate(w http.ResponseWriter, r *http.Request) (domain.Activity, domain.UserID, bool) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return domain.Activity{}, me, false
	}
	actID, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return domain.Activity{}, me, false
	}
	a, err := h.Engagement.Activity(r.Context(), me, actID)
	if err != nil {
		writeActionError(w, err, "not found")
		return domain.Activity{}, me, false
	}
	return a, me, true
}

func (h *EngagementHandlers) listKudos(w http.ResponseWriter, r *http.Request) {
	a, me, ok := h.gate(w, r)
	if !ok {
		return
	}
	v, err := h.Details.Kudos(r.Context(), a, me)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	people := make([]map[string]any, 0, len(v.Kudosers))
	for _, c := range v.Kudosers {
		people = append(people, socialEngagementCardJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": v.Count, "has_kudos": v.HasKudos, "kudosers": people})
}

// socialEngagementCardJSON renders a local user or a federated actor.
func socialEngagementCardJSON(c view.EngagementCard) map[string]any {
	if c.Remote {
		return map[string]any{
			"display_name": c.Handle, "username": c.Handle,
			"remote": true, "actor": c.ActorID,
		}
	}
	if c.User.ID == (domain.UserID{}) {
		return map[string]any{}
	}
	return userCardJSON(c.User)
}

func (h *EngagementHandlers) addKudos(w http.ResponseWriter, r *http.Request) {
	a, me, ok := h.gate(w, r)
	if !ok {
		return
	}
	count, err := h.Engagement.AddKudos(r.Context(), a, me)
	if err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": count, "has_kudos": true})
}

func (h *EngagementHandlers) removeKudos(w http.ResponseWriter, r *http.Request) {
	a, me, ok := h.gate(w, r)
	if !ok {
		return
	}
	count, err := h.Engagement.RemoveKudos(r.Context(), a, me)
	if err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": count, "has_kudos": false})
}

func (h *EngagementHandlers) listComments(w http.ResponseWriter, r *http.Request) {
	a, _, ok := h.gate(w, r)
	if !ok {
		return
	}
	comments, err := h.Details.Comments(r.Context(), a)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(comments))
	for _, c := range comments {
		out = append(out, map[string]any{
			"id":         c.ID,
			"body":       c.Body,
			"created_at": c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			"author":     socialEngagementCardJSON(c.Author),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"comments": out})
}

func (h *EngagementHandlers) addComment(w http.ResponseWriter, r *http.Request) {
	a, me, ok := h.gate(w, r)
	if !ok {
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	id, err := h.Engagement.AddComment(r.Context(), a, me, body.Body)
	if err != nil {
		writeActionError(w, err, "failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id.String()})
}

func (h *EngagementHandlers) deleteComment(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.CommentID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// Author-or-activity-owner is enforced in SQL; anyone else gets 204 too (no info leak).
	if err := h.Engagement.DeleteComment(r.Context(), me, id); err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
