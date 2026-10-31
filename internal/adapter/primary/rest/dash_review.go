package rest

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	matchuc "github.com/opencairn-org/cairn-core/internal/usecase/match"
)

// ReviewHandlers serve the review queue: medium-confidence merges and
// possible duplicates (kind "duplicate", merge via POST /api/activities/merge).
//
//	GET  /api/review-queue
//	POST /api/activities/{id}/reviewed   clear the flag; { duplicate_of } keeps both
type ReviewHandlers struct {
	Auth  *Auth
	Queue *activity.ReviewQueue
	// Links records keep-both decisions (nil → duplicate_of unavailable).
	Links *matchuc.SourceLinks
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
		item := map[string]any{
			"id":               it.ID.String(),
			"title":            it.Title,
			"type":             string(it.Type),
			"start_time":       it.StartTime.UTC(),
			"match_confidence": it.MatchConfidence,
			"kind":             string(it.Kind),
		}
		if it.Kind == domain.ReviewKindDuplicate {
			item["duplicate_of"] = it.DuplicateOf.String()
		}
		out = append(out, item)
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
	var body struct {
		DuplicateOf string `json:"duplicate_of"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // optional; empty or other bodies keep the plain-dismiss behaviour
	if body.DuplicateOf != "" {
		h.keepBoth(w, r, userID, id, body.DuplicateOf)
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

// keepBoth dismisses a possible duplicate for good (cannot-link the pair).
func (h *ReviewHandlers) keepBoth(w http.ResponseWriter, r *http.Request, userID domain.UserID, id domain.ActivityID, rawOther string) {
	other, err := domain.ParseUUID[domain.ActivityID](rawOther)
	if err != nil || other == id {
		http.Error(w, "bad duplicate_of", http.StatusBadRequest)
		return
	}
	if h.Links == nil {
		http.Error(w, "re-clustering engine unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := h.Links.KeepApart(r.Context(), userID, id, other); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.Log.Error("keep both failed", "user_id", userID, "error", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
