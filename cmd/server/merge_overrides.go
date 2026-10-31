package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	matchuc "github.com/opencairn-org/cairn-core/internal/usecase/match"
)

// mergeOverridesAPI holds the dependencies of the manual-override handlers.
type mergeOverridesAPI struct {
	app    *App
	logger *slog.Logger
}

// mountMergeOverrides wires the manual-override surface. Field pins force which
// source wins a field group; merge/split add must/cannot-link constraints and
// recluster.
//
//	GET/PUT/DELETE /api/activities/{id}/field-source(s)   per-field source pin
//	POST           /api/activities/merge                 join (must-link)
//	POST           /api/activities/{id}/split            split (cannot-link)
func mountMergeOverrides(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.FieldOverrides == nil {
		logger.Info("merge-overrides api not mounted: field override repo not wired")
		return
	}
	h := &mergeOverridesAPI{app: app, logger: logger}
	mux.HandleFunc("GET /api/activities/{id}/field-sources", h.listFieldSources)
	mux.HandleFunc("PUT /api/activities/{id}/field-source", h.setFieldSource)
	mux.HandleFunc("DELETE /api/activities/{id}/field-source/{field}", h.deleteFieldSource)
	// Join two activities into one (must-link). Requires the re-clustering engine.
	mux.HandleFunc("POST /api/activities/merge", h.merge)
	// Split a wrongly-merged activity: peel one source off into its own activity
	// (cannot-link it against the activity's other sources, then recluster). The
	// symmetric counterpart to merge/join (brief §13.2).
	mux.HandleFunc("POST /api/activities/{id}/split", h.split)
}

// ownActivity resolves the caller's non-deleted activity or writes an error.
func (h *mergeOverridesAPI) ownActivity(w http.ResponseWriter, r *http.Request, idStr string) (domain.Activity, bool) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return domain.Activity{}, false
	}
	aUUID, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "bad activity id", http.StatusBadRequest)
		return domain.Activity{}, false
	}
	act, err := h.app.Activities.GetActivity(r.Context(), domain.ActivityID(aUUID))
	if err != nil || act.UserID != userID || act.IsDeleted() {
		http.Error(w, "not found", http.StatusNotFound)
		return domain.Activity{}, false
	}
	return act, true
}

func (h *mergeOverridesAPI) listFieldSources(w http.ResponseWriter, r *http.Request) {
	act, ok := h.ownActivity(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	pins, err := h.app.FieldOverrides.ListForActivity(r.Context(), act.ID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]string, 0, len(pins))
	for _, p := range pins {
		out = append(out, map[string]string{"field": string(p.FieldKey), "source_id": p.SourceID.String()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"overrides": out})
}

func (h *mergeOverridesAPI) setFieldSource(w http.ResponseWriter, r *http.Request) {
	act, ok := h.ownActivity(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Field    string `json:"field"`
		SourceID string `json:"source_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	fg := domain.FieldGroup(body.Field)
	if !validFieldGroup(fg) {
		http.Error(w, "unknown field group", http.StatusBadRequest)
		return
	}
	srcUUID, err := uuid.Parse(body.SourceID)
	if err != nil {
		http.Error(w, "bad source id", http.StatusBadRequest)
		return
	}
	// The pinned source must belong to THIS activity.
	src, err := h.app.Activities.GetSource(r.Context(), domain.SourceID(srcUUID))
	if err != nil || src.ActivityID != act.ID {
		http.Error(w, "source not on this activity", http.StatusBadRequest)
		return
	}
	if err := h.app.FieldOverrides.Set(r.Context(), domain.FieldSourceOverride{
		ActivityID: act.ID, FieldKey: fg, SourceID: domain.SourceID(srcUUID),
	}); err != nil {
		http.Error(w, "set failed", http.StatusInternalServerError)
		return
	}
	// Re-derive so the pin takes effect immediately.
	if _, err := h.app.Pipeline.Remerge(r.Context(), act.ID); err != nil {
		h.logger.Warn("recompute after field pin failed", "activity_id", act.ID, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "field": body.Field, "source_id": body.SourceID})
}

func (h *mergeOverridesAPI) deleteFieldSource(w http.ResponseWriter, r *http.Request) {
	act, ok := h.ownActivity(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	fg := domain.FieldGroup(r.PathValue("field"))
	if !validFieldGroup(fg) {
		http.Error(w, "unknown field group", http.StatusBadRequest)
		return
	}
	if err := h.app.FieldOverrides.Delete(r.Context(), act.ID, fg); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	if _, err := h.app.Pipeline.Remerge(r.Context(), act.ID); err != nil {
		h.logger.Warn("recompute after field unpin failed", "activity_id", act.ID, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *mergeOverridesAPI) merge(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	if h.app.SourceLinks == nil {
		http.Error(w, "re-clustering engine unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		ActivityA string `json:"activity_a"`
		ActivityB string `json:"activity_b"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	aA, errA := uuid.Parse(body.ActivityA)
	aB, errB := uuid.Parse(body.ActivityB)
	if errA != nil || errB != nil || aA == aB {
		http.Error(w, "two distinct activity ids required", http.StatusBadRequest)
		return
	}
	if err := h.app.SourceLinks.LinkActivities(r.Context(), userID, domain.ActivityID(aA), domain.ActivityID(aB)); err != nil {
		h.writeLinkError(w, "merge recorded but recluster failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"merged": true})
}

// writeLinkError maps a SourceLinks failure to its HTTP status.
func (h *mergeOverridesAPI) writeLinkError(w http.ResponseWriter, reclusterMsg string, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, matchuc.ErrSourceNotOnActivity):
		http.Error(w, "source not on this activity", http.StatusBadRequest)
	case errors.Is(err, matchuc.ErrSingleSource):
		http.Error(w, "activity has only one source — nothing to split from", http.StatusConflict)
	case errors.Is(err, matchuc.ErrReclusterFailed):
		h.logger.Warn(reclusterMsg, "error", err)
		http.Error(w, reclusterMsg, http.StatusInternalServerError)
	default:
		h.logger.Error("source link failed", "error", err)
		http.Error(w, "link failed", http.StatusInternalServerError)
	}
}

// splitRequest parses the split body and resolves the source to peel; it writes
// the error response on failure.
func (h *mergeOverridesAPI) splitRequest(w http.ResponseWriter, r *http.Request) (domain.ActivityID, domain.UserID, domain.SourceID, bool) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return domain.ActivityID{}, userID, domain.SourceID{}, false
	}
	aUUID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad activity id", http.StatusBadRequest)
		return domain.ActivityID{}, userID, domain.SourceID{}, false
	}
	if !requireJSON(w, r) {
		return domain.ActivityID{}, userID, domain.SourceID{}, false
	}
	if h.app.SourceLinks == nil {
		http.Error(w, "re-clustering engine unavailable", http.StatusServiceUnavailable)
		return domain.ActivityID{}, userID, domain.SourceID{}, false
	}
	var body struct {
		SourceID string `json:"source_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return domain.ActivityID{}, userID, domain.SourceID{}, false
	}
	srcUUID, err := uuid.Parse(body.SourceID)
	if err != nil {
		http.Error(w, "bad source id", http.StatusBadRequest)
		return domain.ActivityID{}, userID, domain.SourceID{}, false
	}
	return domain.ActivityID(aUUID), userID, domain.SourceID(srcUUID), true
}

func (h *mergeOverridesAPI) split(w http.ResponseWriter, r *http.Request) {
	aID, userID, peel, ok := h.splitRequest(w, r)
	if !ok {
		return
	}
	if _, err := h.app.SourceLinks.Split(r.Context(), userID, aID, peel); err != nil {
		h.writeLinkError(w, "split recorded but recluster failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"split": true})
}

// validFieldGroup reports whether fg is a real, user-pinnable field group.
func validFieldGroup(fg domain.FieldGroup) bool {
	for _, g := range domain.AllFieldGroups {
		if g == fg {
			return true
		}
	}
	return false
}
