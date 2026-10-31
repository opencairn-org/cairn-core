package rest

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	"github.com/opencairn-org/cairn-core/internal/usecase/pipeline"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// ActivityManageHandlers back the /activities/{id}/manage page: a technical
// insights view plus the in-process maintenance actions.
//
//	GET  /api/activities/{id}/manage             provenance + per-source insights
//	POST /api/activities/{id}/recompute          re-merge from current sources
//	POST /api/activities/{id}/recompute-derived  best-efforts + segments + training-load
type ActivityManageHandlers struct {
	Auth     *Auth
	Actions  *activity.SourceActions
	Details  *view.Details
	Pipeline *pipeline.Pipeline
	Log      *slog.Logger
}

// Mount registers the routes.
func (h *ActivityManageHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/activities/{id}/manage", h.insights)
	mux.HandleFunc("POST /api/activities/{id}/recompute", h.recompute)
	mux.HandleFunc("POST /api/activities/{id}/recompute-derived", h.recomputeDerived)
}

// actOwnedActivity resolves the caller's live {id} activity or writes an error.
func actOwnedActivity(w http.ResponseWriter, r *http.Request, auth *Auth, actions *activity.SourceActions) (domain.Activity, bool) {
	me, ok := auth.requireUser(w, r)
	if !ok {
		return domain.Activity{}, false
	}
	id, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad activity id", http.StatusBadRequest)
		return domain.Activity{}, false
	}
	act, err := actions.OwnedActivity(r.Context(), me, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return domain.Activity{}, false
	}
	return act, true
}

func (h *ActivityManageHandlers) insights(w http.ResponseWriter, r *http.Request) {
	act, ok := actOwnedActivity(w, r, h.Auth, h.Actions)
	if !ok {
		return
	}
	info, err := h.Details.Manage(r.Context(), act)
	if err != nil {
		h.Log.Error("manage: list sources failed", "activity_id", act.ID, "error", err)
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	primary := ""
	if info.Primary != nil {
		primary = info.Primary.String()
	}
	sources := make([]map[string]any, 0, len(info.Sources))
	for _, in := range info.Sources {
		sources = append(sources, actManageSourceJSON(in))
	}
	provenance := make([]map[string]any, 0, len(info.Provenance))
	for _, p := range info.Provenance {
		provenance = append(provenance, map[string]any{
			"field":       string(p.Field),
			"field_label": p.FieldLabel,
			"source_id":   p.SourceID.String(),
			"provider":    p.Provider,
			"decided_by":  p.DecidedBy,
			"synced_at":   act.MergedAt.UTC(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activity": map[string]any{
			"id":           act.ID.String(),
			"title":        act.Title,
			"type":         string(act.Type),
			"start_time":   act.StartTime.UTC(),
			"merged_at":    act.MergedAt.UTC(),
			"timezone":     act.Timezone,
			"source_count": len(info.Sources),
			"primary":      primary,
		},
		"sources":    sources,
		"provenance": provenance,
	})
}

// actManageSourceJSON renders one source row of the manage view.
func actManageSourceJSON(in domain.SourceInsight) map[string]any {
	s := in.Source
	var extAcct string
	if s.ExternalAccountID != nil {
		extAcct = s.ExternalAccountID.String()
	}
	var lastReimport any
	if s.LastReimportedAt != nil {
		lastReimport = s.LastReimportedAt.UTC()
	}
	return map[string]any{
		"id":                 s.ID.String(),
		"provider":           s.Provider,
		"external_id":        s.ExternalID,
		"external_account":   extAcct,
		"worker_name":        s.SourceWorkerName,
		"worker_version":     s.SourceWorkerVersion,
		"worker_release":     s.SourceWorkerRelease,
		"parser_version":     in.ParserVersion,
		"dataset_version":    in.DatasetVersion,
		"reparse_with":       in.ReparseWith,
		"upgrade":            in.Upgrade,
		"worker_package":     s.SourceWorkerPackage,
		"reparse_eligible":   in.ReparseEligible,
		"has_blob":           s.RawBlobID != "",
		"raw_content_type":   s.RawContentType,
		"raw_size_bytes":     s.RawSizeBytes,
		"has_stream":         s.Parsed.HasStream,
		"lap_count":          len(s.Parsed.Laps),
		"status":             string(s.Status),
		"status_reason":      s.StatusReason,
		"reimport_status":    string(s.ReimportStatus),
		"reimport_reason":    s.ReimportStatusReason,
		"imported_at":        s.ImportedAt.UTC(),
		"last_reimported_at": lastReimport,
		"updated_at":         s.UpdatedAt.UTC(),
		"is_primary":         in.IsPrimary,
		"won_field_groups":   in.WonFieldGroups,
	}
}

func (h *ActivityManageHandlers) recompute(w http.ResponseWriter, r *http.Request) {
	act, ok := actOwnedActivity(w, r, h.Auth, h.Actions)
	if !ok {
		return
	}
	// Remerge retracts a soft-deleted activity from remote followers.
	result, err := h.Pipeline.Remerge(r.Context(), act.ID)
	if err != nil {
		h.Log.Error("manage: recompute failed", "activity_id", act.ID, "error", err)
		http.Error(w, "recompute failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"recomputed":   true,
		"soft_deleted": result.SoftDeleted,
		"source_count": len(result.SourceIDs),
	})
}

// recomputeDerived re-runs every source's derived data plus training load;
// failures are reported, not fatal.
func (h *ActivityManageHandlers) recomputeDerived(w http.ResponseWriter, r *http.Request) {
	act, ok := actOwnedActivity(w, r, h.Auth, h.Actions)
	if !ok {
		return
	}
	n, warnings, err := h.Pipeline.RecomputeDerived(r.Context(), act)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"recomputed": false, "warnings": []string{"list_sources: " + err.Error()}})
		return
	}
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"recomputed": true, "source_count": n, "warnings": warnings})
}
