package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"

	workerv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/worker/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/protoconv"
)

// heldResult is what a preview stores: the worker's activity message plus
// the provenance the JobResult envelope carried, enough to ingest later.
type heldResult struct {
	Activity      json.RawMessage `json:"activity"`
	WorkerName    string          `json:"worker_name"`
	WorkerVersion string          `json:"worker_version"`
	WorkerPackage string          `json:"worker_package"`
	WorkerRelease string          `json:"worker_release"`
}

// capturePreview hands a reimport result to a pending preview of its source.
// claimed reports whether the result was held (and must not be ingested).
func capturePreview(ctx context.Context, app *App, log *slog.Logger, job string, jr *workerv1.JobResult, ia *workerv1.ImportedActivity) (claimed bool) {
	if app.SourcePreviews == nil || (job != "parse_blob" && job != "fetch_source") {
		return false
	}
	ref := ia.GetRef()
	_, acct, err := ingestActivityParseRef(ref)
	if err != nil {
		return false
	}
	src, err := app.Activities.FindSourceByExternalID(ctx, ref.GetProvider(), acct, ref.GetExternalId())
	if err != nil {
		return false
	}
	body, err := protojson.Marshal(ia)
	if err != nil {
		return false
	}
	raw, _ := json.Marshal(heldResult{
		Activity: body, WorkerName: jr.GetWorkerName(), WorkerVersion: jr.GetWorkerVersion(),
		WorkerPackage: jr.GetWorkerPackage(), WorkerRelease: jr.GetWorkerRelease(),
	})
	payload := protoconv.ActivitySourcePayloadFromProto(ia.GetPayload())
	payload.HasStream = ia.GetStream().GetSampleCount() > 0
	claimed, err = app.SourcePreviews.Capture(ctx, src, payload, raw)
	if err != nil {
		log.Warn("preview capture failed; result ingested instead", "source_id", src.ID, "error", err)
		return false
	}
	return claimed
}

// previewCommitter ingests a held preview result (port.SourcePreviewCommitter).
type previewCommitter struct {
	app    *App
	logger *slog.Logger
}

var _ port.SourcePreviewCommitter = (*previewCommitter)(nil)

func (c *previewCommitter) Commit(ctx context.Context, userID domain.UserID, id domain.SourcePreviewID) (domain.Activity, error) {
	prev, err := c.app.SourcePreviews.TakeForCommit(ctx, userID, id)
	if err != nil {
		return domain.Activity{}, err
	}
	var held heldResult
	if err := json.Unmarshal(prev.Parsed, &held); err != nil {
		return domain.Activity{}, port.NewActionError(port.ActionErrorInternal, "held result is unreadable")
	}
	ia := &workerv1.ImportedActivity{}
	if err := protojson.Unmarshal(held.Activity, ia); err != nil {
		return domain.Activity{}, port.NewActionError(port.ActionErrorInternal, "held result is unreadable")
	}
	jr := &workerv1.JobResult{WorkerName: held.WorkerName, WorkerVersion: held.WorkerVersion, WorkerPackage: held.WorkerPackage, WorkerRelease: held.WorkerRelease}
	out, err := ingestActivityEvent(ctx, c.app, c.logger, jr, ia)
	if err != nil {
		return domain.Activity{}, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("commit preview: %v", err))
	}
	return out.Activity, nil
}

// mountSourcePreviews is the REST twin of the preview RPCs:
//
//	POST   /api/activities/{id}/sources/{sourceId}/preview  { mode: "refetch"|"reparse" }
//	GET    /api/source-previews/{id}
//	POST   /api/source-previews/{id}/commit
//	DELETE /api/source-previews/{id}
func mountSourcePreviews(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.SourcePreviews == nil {
		return
	}
	h := &previewHandlers{app: app, committer: &previewCommitter{app: app, logger: logger}}
	mux.HandleFunc("POST /api/activities/{id}/sources/{sourceId}/preview", h.start)
	mux.HandleFunc("GET /api/source-previews/{id}", h.get)
	mux.HandleFunc("POST /api/source-previews/{id}/commit", h.commit)
	mux.HandleFunc("DELETE /api/source-previews/{id}", h.discard)
}

type previewHandlers struct {
	app       *App
	committer *previewCommitter
}

func (h *previewHandlers) start(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	aID, sID, ok := activitySourcesParseIDs(w, r)
	if !ok {
		return
	}
	mode, err := activitySourcesReimportMode(r)
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	prev, err := h.app.SourcePreviews.Start(r.Context(), userID, aID, sID, mode == "reparse")
	if err != nil {
		writeActionError(w, err, "preview failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"preview": previewJSON(prev)})
}

// previewID resolves the caller and the path id.
func (h *previewHandlers) previewID(w http.ResponseWriter, r *http.Request) (domain.UserID, domain.SourcePreviewID, bool) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return domain.UserID{}, domain.SourcePreviewID{}, false
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return domain.UserID{}, domain.SourcePreviewID{}, false
	}
	return userID, domain.SourcePreviewID(id), true
}

func (h *previewHandlers) get(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.previewID(w, r)
	if !ok {
		return
	}
	prev, err := h.app.SourcePreviews.Get(r.Context(), userID, id)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"preview": previewJSON(prev)})
}

func (h *previewHandlers) commit(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.previewID(w, r)
	if !ok {
		return
	}
	act, err := h.committer.Commit(r.Context(), userID, id)
	if err != nil {
		writeActionError(w, err, "commit failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"activity_id": act.ID.String(), "committed": true})
}

func (h *previewHandlers) discard(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.previewID(w, r)
	if !ok {
		return
	}
	if err := h.app.SourcePreviews.Discard(r.Context(), userID, id); err != nil {
		writeActionError(w, err, "discard failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func previewJSON(p domain.SourcePreview) map[string]any {
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
