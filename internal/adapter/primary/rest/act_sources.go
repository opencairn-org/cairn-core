package rest

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
)

// ActivitySourceHandlers are the owner's per-source actions on an activity.
// Detach undoes a wrong heuristic attach: the source stays stored for audit
// but leaves the merge, which re-runs (or soft-deletes the activity when it
// was the last source).
//
//	DELETE /api/activities/{id}                                  soft-delete (detaches every source)
//	GET    /api/activities/{id}/sources/{sourceId}/parsed        stored normalized payload
//	POST   /api/activities/{id}/sources/{sourceId}/detach        { reason? }
//	GET    /api/activities/{id}/sources/{sourceId}/download      archived original file
//	POST   /api/activities/{id}/sources/{sourceId}/reimport      { mode: "refetch"|"reparse" }
type ActivitySourceHandlers struct {
	Auth    *Auth
	Actions *activity.SourceActions
	// Blobs serves the archived originals (nil → download unavailable).
	Blobs port.BlobStore
	// Reimporter queues re-fetch / re-parse jobs; the worker result then flows
	// through the normal ingest path and resets reimport_status.
	Reimporter port.SourceReimporter
	Log        *slog.Logger
}

// Mount registers the routes.
func (h *ActivitySourceHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("DELETE /api/activities/{id}", h.deleteActivity)
	mux.HandleFunc("GET /api/activities/{id}/sources/{sourceId}/parsed", h.parsed)
	mux.HandleFunc("POST /api/activities/{id}/sources/{sourceId}/detach", h.detach)
	// Proxied, not redirected: the store's endpoint need not be browser-reachable.
	mux.HandleFunc("GET /api/activities/{id}/sources/{sourceId}/download", h.download)
	mux.HandleFunc("POST /api/activities/{id}/sources/{sourceId}/reimport", h.reimport)
}

// actSourceIDs parses the {id} and {sourceId} path values, writing 400 on failure.
func actSourceIDs(w http.ResponseWriter, r *http.Request) (domain.ActivityID, domain.SourceID, bool) {
	aID, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad activity id", http.StatusBadRequest)
		return aID, domain.SourceID{}, false
	}
	sID, err := domain.ParseUUID[domain.SourceID](r.PathValue("sourceId"))
	if err != nil {
		http.Error(w, "bad source id", http.StatusBadRequest)
		return aID, sID, false
	}
	return aID, sID, true
}

// actReimportMode reads the optional {mode} body, defaulting to "refetch".
func actReimportMode(r *http.Request) (string, error) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := decodeOptionalJSONBody(r, &body); err != nil {
		return "", err
	}
	if body.Mode == "" {
		return "refetch", nil
	}
	return body.Mode, nil
}

// source loads one source of the caller's activity, writing 404 on any miss.
func (h *ActivitySourceHandlers) source(w http.ResponseWriter, r *http.Request, me domain.UserID, aID domain.ActivityID, sID domain.SourceID, withDeleted bool) (domain.ActivitySource, bool) {
	src, err := h.Actions.SourceOf(r.Context(), me, aID, sID, withDeleted)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return src, false
	}
	return src, true
}

// writeError maps a source-action failure to its HTTP status.
func (h *ActivitySourceHandlers) writeError(w http.ResponseWriter, msg string, err error) {
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, activity.ErrSourceNotOnActivity) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.Log.Error(msg, "error", err)
	http.Error(w, msg, http.StatusInternalServerError)
}

func (h *ActivitySourceHandlers) deleteActivity(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	aID, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad activity id", http.StatusBadRequest)
		return
	}
	if err := h.Actions.Delete(r.Context(), me, aID); err != nil {
		h.writeError(w, "delete activity failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "activity_id": aID.String()})
}

func (h *ActivitySourceHandlers) parsed(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	aID, sID, ok := actSourceIDs(w, r)
	if !ok {
		return
	}
	src, ok := h.source(w, r, me, aID, sID, true)
	if !ok {
		return
	}
	raw, err := h.Actions.ParsedRaw(r.Context(), src)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw) //nolint:gosec // G705: stored JSON served as application/json, owner-only
}

func (h *ActivitySourceHandlers) detach(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	aID, sID, ok := actSourceIDs(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeOptionalJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	res, err := h.Actions.Detach(r.Context(), me, aID, sID, strings.TrimSpace(body.Reason))
	if err != nil {
		h.writeError(w, "detach source failed", err)
		return
	}
	if res.RecomputeErr != nil {
		// The detach is durable; the re-merge is best-effort.
		h.Log.Warn("detach succeeded but recompute failed", "activity_id", aID, "error", res.RecomputeErr)
		writeJSON(w, http.StatusOK, map[string]any{
			"detached":        true,
			"recomputed":      false,
			"recompute_error": res.RecomputeErr.Error(),
			"activity_id":     aID.String(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"detached":     true,
		"recomputed":   true,
		"soft_deleted": res.SoftDeleted,
		"source_count": res.SourceCount,
		"activity_id":  aID.String(),
	})
}

func (h *ActivitySourceHandlers) download(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if h.Blobs == nil {
		http.Error(w, "object storage is not configured", http.StatusServiceUnavailable)
		return
	}
	aID, sID, ok := actSourceIDs(w, r)
	if !ok {
		return
	}
	src, ok := h.source(w, r, me, aID, sID, false)
	if !ok {
		return
	}
	if src.RawBlobID == "" {
		http.Error(w, "no archived original for this source", http.StatusNotFound)
		return
	}
	data, ct, err := h.Blobs.Get(r.Context(), src.RawBlobID)
	if err != nil {
		h.Log.Error("blob get failed", "source_id", sID, "error", err)
		http.Error(w, "download unavailable", http.StatusBadGateway)
		return
	}
	if ct == "" {
		ct = src.RawContentType
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	ext := ""
	if i := strings.LastIndex(src.RawBlobID, "."); i >= 0 {
		ext = src.RawBlobID[i:]
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+src.Provider+"-"+src.ExternalID+ext+"\"")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (h *ActivitySourceHandlers) reimport(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	aID, sID, ok := actSourceIDs(w, r)
	if !ok {
		return
	}
	src, ok := h.source(w, r, me, aID, sID, false)
	if !ok {
		return
	}
	// "refetch" pulls fresh from the provider; "reparse" re-runs the worker's
	// mapping over the archived blob (no provider quota).
	mode, err := actReimportMode(r)
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if mode == "reparse" {
		_, err = h.Reimporter.Reparse(r.Context(), me, src)
	} else {
		_, err = h.Reimporter.Refetch(r.Context(), me, src)
	}
	if err != nil {
		writeActionError(w, err, "re-import failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": true, "mode": mode, "provider": src.Provider})
}

// SourcePreviewHandlers are the REST twin of the reimport dry-run RPCs.
//
//	POST   /api/activities/{id}/sources/{sourceId}/preview  { mode: "refetch"|"reparse" }
//	GET    /api/source-previews/{id}
//	POST   /api/source-previews/{id}/commit
//	DELETE /api/source-previews/{id}
type SourcePreviewHandlers struct {
	Auth      *Auth
	Previews  *activity.SourcePreviews
	Committer port.SourcePreviewCommitter
}

// Mount registers the routes.
func (h *SourcePreviewHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/activities/{id}/sources/{sourceId}/preview", h.start)
	mux.HandleFunc("GET /api/source-previews/{id}", h.get)
	mux.HandleFunc("POST /api/source-previews/{id}/commit", h.commit)
	mux.HandleFunc("DELETE /api/source-previews/{id}", h.discard)
}

func (h *SourcePreviewHandlers) start(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	aID, sID, ok := actSourceIDs(w, r)
	if !ok {
		return
	}
	mode, err := actReimportMode(r)
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	prev, err := h.Previews.Start(r.Context(), me, aID, sID, mode == "reparse")
	if err != nil {
		writeActionError(w, err, "preview failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"preview": actPreviewJSON(prev)})
}

// previewID resolves the caller and the path id.
func (h *SourcePreviewHandlers) previewID(w http.ResponseWriter, r *http.Request) (domain.UserID, domain.SourcePreviewID, bool) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return me, domain.SourcePreviewID{}, false
	}
	id, err := domain.ParseUUID[domain.SourcePreviewID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return me, id, false
	}
	return me, id, true
}

func (h *SourcePreviewHandlers) get(w http.ResponseWriter, r *http.Request) {
	me, id, ok := h.previewID(w, r)
	if !ok {
		return
	}
	prev, err := h.Previews.Get(r.Context(), me, id)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"preview": actPreviewJSON(prev)})
}

func (h *SourcePreviewHandlers) commit(w http.ResponseWriter, r *http.Request) {
	me, id, ok := h.previewID(w, r)
	if !ok {
		return
	}
	act, err := h.Committer.Commit(r.Context(), me, id)
	if err != nil {
		writeActionError(w, err, "commit failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"activity_id": act.ID.String(), "committed": true})
}

func (h *SourcePreviewHandlers) discard(w http.ResponseWriter, r *http.Request) {
	me, id, ok := h.previewID(w, r)
	if !ok {
		return
	}
	if err := h.Previews.Discard(r.Context(), me, id); err != nil {
		writeActionError(w, err, "discard failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func actPreviewJSON(p domain.SourcePreview) map[string]any {
	changes := func(in []domain.FieldChange) []map[string]string {
		out := make([]map[string]string, 0, len(in))
		for _, c := range in {
			out = append(out, map[string]string{"field": c.Field, "before": c.Before, "after": c.After})
		}
		return out
	}
	return map[string]any{
		"id": p.ID.String(), "activity_source_id": p.SourceID.String(), "activity_id": p.ActivityID.String(),
		"job_id": p.JobID, "status": string(p.Status),
		"source_diff": changes(p.SourceDiff), "merged_diff": changes(p.MergedDiff),
		"created_at": p.CreatedAt.UTC().Format(time.RFC3339), "expires_at": p.ExpiresAt.UTC().Format(time.RFC3339),
	}
}
