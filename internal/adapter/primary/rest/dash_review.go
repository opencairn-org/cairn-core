package rest

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
)

// ReviewHandlers serve the confidence-band review queue.
//
//	GET  /api/review-queue
//	POST /api/activities/{id}/reviewed   clear the flag
type ReviewHandlers struct {
	Auth  *Auth
	Queue *activity.ReviewQueue
	Log   *slog.Logger
}

// Mount registers the routes.
func (h *ReviewHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/review-queue", h.list)
	mux.HandleFunc("POST /api/activities/{id}/reviewed", h.reviewed)
}

func (h *ReviewHandlers) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	items, err := h.Queue.List(r.Context(), userID)
	if err != nil {
		h.Log.Error("review queue list failed", "user_id", userID, "error", err)
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"id":               it.ID.String(),
			"title":            it.Title,
			"type":             string(it.Type),
			"start_time":       it.StartTime.UTC(),
			"match_confidence": it.MatchConfidence,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *ReviewHandlers) reviewed(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad activity id", http.StatusBadRequest)
		return
	}
	if err := h.Queue.MarkReviewed(r.Context(), userID, id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
