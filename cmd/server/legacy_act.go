package main

// Still used outside the activity-writes group: the worker-presence reads,
// the reimport dispatcher and preview capture/commit (Connect, sync, ingest),
// and activityManageLoadOwned (activity sub-resource handlers).

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
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/protoconv"
)

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

// sourceReimporter implements port.SourceReimporter over the NATS job bus and
// worker presence, so REST and Connect share one dispatch path.
type sourceReimporter struct {
	app    *App
	logger *slog.Logger
}

func newSourceReimporter(app *App, logger *slog.Logger) *sourceReimporter {
	return &sourceReimporter{app: app, logger: logger}
}

var _ port.SourceReimporter = (*sourceReimporter)(nil)

// Refetch queues a fetch_source job for the source's provider on the
// account's pool. Needs an external account (not a manual upload), a bus and
// a resolvable pool; token validity and provider budget surface later as the
// job sitting in the queue.
func (ri *sourceReimporter) Refetch(ctx context.Context, userID domain.UserID, src domain.ActivitySource) (string, error) {
	app, logger := ri.app, ri.logger
	if app.NATSBus == nil {
		return "", port.NewActionError(port.ActionErrorUnavailable, "re-fetch requires the async worker bus, which isn't configured")
	}
	if src.ExternalAccountID == nil || src.ExternalID == "" {
		return "", port.NewActionError(port.ActionErrorPrecondition, "this source has no external provider to re-fetch from")
	}
	// The account's pool re-fetches; a reassigned account may switch the
	// source to another worker package (docs/adr/0006).
	acct, err := app.ExternalAccounts.GetExternalAccount(ctx, *src.ExternalAccountID)
	if err != nil {
		return "", port.NewActionError(port.ActionErrorNotFound, "account not found")
	}
	subject, err := accountJobSubject(ctx, app, logger, "fetch_source", acct)
	if err != nil {
		return "", port.NewActionError(port.ActionErrorConflict, err.Error())
	}

	// Flip to 'updating' first so a concurrent view sees the in-flight state.
	if err := app.Activities.SetSourceReimportStatus(ctx, src.ID, domain.ReimportStatusUpdating, "user_refetch"); err != nil {
		logger.Error("set reimport status failed", "source_id", src.ID, "error", err)
		return "", fmt.Errorf("set reimport status: %w", err)
	}

	jobID := "refetch:" + src.ID.String()
	ri.publishStarted(userID, src, jobID, "refetch")
	body, _ := json.Marshal(map[string]any{
		"job_id":        jobID,
		"account_id":    src.ExternalAccountID.String(),
		"user_id":       userID.String(),
		"provider":      src.Provider,
		"ext_id":        src.ExternalID,
		"fetch_streams": true,
		"reason":        "user_refetch",
	})
	// Deterministic msg-id collapses double-clicks within the dedup window.
	msgID := "refetch:" + src.Provider + ":" + src.ExternalID
	if err := app.NATSBus.Publish(ctx, subject, msgID, body); err != nil {
		// Roll the status back so the user can retry; the job never landed.
		_ = app.Activities.SetSourceReimportStatus(ctx, src.ID, domain.ReimportStatusCurrent, "")
		logger.Error("publish fetch_source failed", "source_id", src.ID, "error", err)
		return "", port.NewActionError(port.ActionErrorUpstream, "could not queue re-fetch")
	}
	return jobID, nil
}

// Reparse mints a presigned download URL for the source's archived raw blob
// and publishes a parse_blob.<provider> job. A worker re-runs its mapping over
// the archive (no provider API quota) and publishes a result that flows
// through the normal ingest pipeline → handleReimport replaces the payload.
func (ri *sourceReimporter) Reparse(ctx context.Context, userID domain.UserID, src domain.ActivitySource) (string, error) {
	app, logger := ri.app, ri.logger
	if app.NATSBus == nil {
		return "", port.NewActionError(port.ActionErrorUnavailable, "re-parse requires the async worker bus, which isn't configured")
	}
	if app.BlobStore == nil {
		return "", port.NewActionError(port.ActionErrorUnavailable, "object storage isn't configured, so re-parse is unavailable")
	}
	if src.RawBlobID == "" {
		return "", port.NewActionError(port.ActionErrorPrecondition, "this source has no archived original to re-parse — re-fetch from the provider first")
	}
	// Re-parse goes to a pool of the source's own package, which need not be
	// the account's assigned one (docs/adr/0006).
	rw, ok := reparseWorker(currentWorkers(ctx, app, logger), src)
	if !ok {
		return "", port.NewActionError(port.ActionErrorConflict, "no compatible worker is online to re-parse this source (needs the same dataset and a parser at least as new)")
	}

	signed, err := app.BlobStore.PresignDownload(ctx, src.RawBlobID, port.PresignDownloadOpts{})
	if err != nil {
		logger.Error("presign download for reparse failed", "source_id", src.ID, "error", err)
		return "", port.NewActionError(port.ActionErrorUpstream, "could not prepare the archived blob for re-parse")
	}

	var acctID string
	if src.ExternalAccountID != nil {
		acctID = src.ExternalAccountID.String()
	}
	jobID := "reparse:" + src.ID.String()
	ri.publishStarted(userID, src, jobID, "reparse")
	job := map[string]any{
		"job_id":     jobID,
		"source_id":  src.ID.String(),
		"user_id":    userID.String(),
		"provider":   src.Provider,
		"account_id": acctID,
		"ext_id":     src.ExternalID,
		// A pooled worker with an older parser NAKs (docs/adr/0005).
		"min_parser_version": minParserVersion(src),
		"blob": map[string]any{
			"url":             signed.URL,
			"expires_at":      signed.ExpiresAt,
			"fallback_handle": src.RawBlobID, // worker re-presigns from this key if expired
		},
	}
	body, _ := json.Marshal(job)

	if err := app.Activities.SetSourceReimportStatus(ctx, src.ID, domain.ReimportStatusUpdating, "user_reparse"); err != nil {
		logger.Error("set reimport status failed", "source_id", src.ID, "error", err)
		return "", fmt.Errorf("set reimport status: %w", err)
	}
	msgID := "reparse:" + src.Provider + ":" + src.ExternalID
	if err := app.NATSBus.Publish(ctx, domain.JobSubject("parse_blob", src.Provider, rw.name), msgID, body); err != nil {
		_ = app.Activities.SetSourceReimportStatus(ctx, src.ID, domain.ReimportStatusCurrent, "")
		logger.Error("publish parse_blob failed", "source_id", src.ID, "error", err)
		return "", port.NewActionError(port.ActionErrorUpstream, "could not queue re-parse")
	}
	return jobID, nil
}

// minParserVersion is the source's parser version as N.M, or the raw string
// when it is not one.
func minParserVersion(src domain.ActivitySource) string {
	if v, err := domain.ParseParserVersion(src.SourceWorkerVersion); err == nil {
		return v.String()
	}
	return src.SourceWorkerVersion
}

// publishStarted emits the live reimport-started event.
func (ri *sourceReimporter) publishStarted(userID domain.UserID, src domain.ActivitySource, jobID, mode string) {
	if ri.app.ActivityEvents == nil {
		return
	}
	ri.app.ActivityEvents.Publish(domain.NewActivityEvent(domain.ActivityEventReimportStarted, userID, "activity.reimport_started").
		For(src.ActivityID, src.ID).With("provider", src.Provider).With("job_id", jobID).With("mode", mode))
}

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
