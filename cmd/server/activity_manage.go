package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
)

// mountActivityManage wires the user-facing activity "manage" surface: a
// read-only technical-insights view plus the maintenance actions that run
// fully in-process today (re-merge from sources, recompute derived data).
// Source-level detach + raw download live in activity_sources.go; together
// they back the /activities/{id}/manage page.
//
//	GET  /api/activities/{id}/manage             → provenance + per-source insights
//	POST /api/activities/{id}/recompute          → re-merge from current sources
//	POST /api/activities/{id}/recompute-derived  → best-efforts + segments + training-load
func mountActivityManage(mux *http.ServeMux, app *App, logger *slog.Logger) {
	mux.HandleFunc("GET /api/activities/{id}/manage", activityManageInsightsHandler(app, logger))
	mux.HandleFunc("POST /api/activities/{id}/recompute", activityManageRecomputeHandler(app, logger))
	mux.HandleFunc("POST /api/activities/{id}/recompute-derived", activityManageRecomputeDerivedHandler(app, logger))

	logger.Info("activity manage endpoints mounted")
}

// activityManageLoadOwned resolves the activity and enforces ownership; returns false
// (and writes the response) on any failure.
func activityManageLoadOwned(w http.ResponseWriter, r *http.Request, app *App) (domain.Activity, bool) {
	userID, ok := resolveSessionUser(r, app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return domain.Activity{}, false
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad activity id", http.StatusBadRequest)
		return domain.Activity{}, false
	}
	act, err := app.Activities.GetActivity(r.Context(), domain.ActivityID(id))
	if err != nil || act.UserID != userID || act.IsDeleted() {
		http.Error(w, "not found", http.StatusNotFound)
		return domain.Activity{}, false
	}
	return act, true
}

func activityManageInsightsHandler(app *App, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		act, ok := activityManageLoadOwned(w, r, app)
		if !ok {
			return
		}
		info, err := app.Details.Manage(r.Context(), act)
		if err != nil {
			logger.Error("manage: list sources failed", "activity_id", act.ID, "error", err)
			http.Error(w, "load failed", http.StatusInternalServerError)
			return
		}
		primary := ""
		if info.Primary != nil {
			primary = info.Primary.String()
		}
		sources := make([]map[string]any, 0, len(info.Sources))
		for _, in := range info.Sources {
			sources = append(sources, activityManageSourceJSON(in))
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
}

// activityManageSourceJSON renders one source row of the manage view.
func activityManageSourceJSON(in domain.SourceInsight) map[string]any {
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

func activityManageRecomputeHandler(app *App, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		act, ok := activityManageLoadOwned(w, r, app)
		if !ok {
			return
		}
		// Remerge retracts a soft-deleted activity from remote followers.
		result, err := app.Pipeline.Remerge(r.Context(), act.ID)
		if err != nil {
			logger.Error("manage: recompute failed", "activity_id", act.ID, "error", err)
			http.Error(w, "recompute failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"recomputed":   true,
			"soft_deleted": result.SoftDeleted,
			"source_count": len(result.SourceIDs),
		})
	}
}

func activityManageRecomputeDerivedHandler(app *App, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		act, ok := activityManageLoadOwned(w, r, app)
		if !ok {
			return
		}
		res := recomputeDerivedForActivity(r.Context(), app, logger, act)
		writeJSON(w, http.StatusOK, res)
	}
}

// liveWorker is the identity of a currently-online worker that matters for
// re-parse: provider + version + package. Those three ARE the compatibility
// contract — the maintainer guarantees that a given (provider, package, version)
// triple parses a blob the same way. Worker NAME/alias is deliberately NOT part
// of the identity: names are admin-assigned routing labels, not code identity.
type liveWorker struct {
	name         string // pool name: jobs go to cairn.jobs.<job>.<provider>.<name>
	provider     string
	version      string // parser version N.M
	pkg          string
	release      string
	compatible   bool // release matches core's major.minor (or either is untagged)
	capabilities capability.Manifest
}

// currentWorkers reads the worker-presence KV and returns the identity of every
// live worker. Empty when NATS isn't wired / nothing is online — callers treat
// that as "no compatible worker, re-parse not possible".
func currentWorkers(ctx context.Context, app *App, log *slog.Logger) []liveWorker {
	return app.presence.get(time.Now(), func() []liveWorker { return scanWorkers(ctx, app, log) })
}

func scanWorkers(ctx context.Context, app *App, log *slog.Logger) []liveWorker {
	var out []liveWorker
	if app.NATSBus == nil {
		return out
	}
	kv, err := app.NATSBus.KV("cairn_worker_presence")
	if err != nil {
		log.Warn("live workers: presence bucket unavailable", "error", err)
		return out
	}
	keys, _ := kv.Keys(ctx)
	now := time.Now()
	for _, k := range keys {
		entry, err := kv.Get(ctx, k)
		if err != nil {
			// Presence keys carry a TTL; one can expire between Keys and Get.
			log.Debug("live workers: presence key vanished", "key", k, "error", err)
			continue
		}
		var hb struct {
			WorkerName   string              `json:"worker_name"`
			Provider     string              `json:"provider"`
			Version      string              `json:"version"`
			Release      string              `json:"release"`
			Package      string              `json:"package"`
			LastSeen     string              `json:"last_seen"`
			Capabilities capability.Manifest `json:"capabilities"`
		}
		if json.Unmarshal(entry.Value, &hb) != nil {
			continue
		}
		if ts, perr := time.Parse(time.RFC3339, hb.LastSeen); perr == nil && now.Sub(ts) > presenceStaleAfter {
			continue // stale ghost
		}
		compatible, _ := domain.ReleaseCompatible(Version, hb.Release)
		out = append(out, liveWorker{
			name: hb.WorkerName, provider: hb.Provider, version: hb.Version, pkg: hb.Package,
			release: hb.Release, compatible: compatible, capabilities: hb.Capabilities,
		})
	}
	return out
}

// domainWorkers converts the presence records to the domain identity.
func domainWorkers(ws []liveWorker) []domain.LiveWorker {
	out := make([]domain.LiveWorker, 0, len(ws))
	for _, w := range ws {
		out = append(out, domain.LiveWorker{Name: w.name, Provider: w.provider, Version: w.version, Package: w.pkg, Release: w.release, Compatible: w.compatible})
	}
	return out
}

// reparseWorker is domain.ReparseWorker over the presence records (docs/adr/0005).
func reparseWorker(workers []liveWorker, src domain.ActivitySource) (liveWorker, bool) {
	dw, ok := domain.ReparseWorker(domainWorkers(workers), src)
	if !ok {
		return liveWorker{}, false
	}
	for _, w := range workers {
		if w.name == dw.Name && w.version == dw.Version && w.pkg == dw.Package {
			return w, true
		}
	}
	return liveWorker{}, false
}

// workerPresence implements port.WorkerPresence over the presence cache.
type workerPresence struct {
	app    *App
	logger *slog.Logger
}

func (p workerPresence) LiveWorkers(ctx context.Context) []domain.LiveWorker {
	return domainWorkers(currentWorkers(ctx, p.app, p.logger))
}

// recomputeDerivedForActivity re-runs the derived computations of every
// source plus the user's training load; failures are reported, not fatal.
func recomputeDerivedForActivity(ctx context.Context, app *App, _ *slog.Logger, act domain.Activity) map[string]any {
	n, warnings, err := app.Pipeline.RecomputeDerived(ctx, act)
	if err != nil {
		return map[string]any{"recomputed": false, "warnings": []string{"list_sources: " + err.Error()}}
	}
	if warnings == nil {
		warnings = []string{}
	}
	return map[string]any{"recomputed": true, "source_count": n, "warnings": warnings}
}
