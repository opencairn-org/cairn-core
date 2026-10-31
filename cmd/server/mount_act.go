package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	workerv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/worker/v1"
	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/fileimport"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	"github.com/opencairn-org/cairn-core/internal/usecase/athlete"
	"github.com/opencairn-org/cairn-core/internal/usecase/instance"
)

// mountAct mounts the activity write endpoints: imports, source actions and
// previews, the manage page, merge overrides and policies, the athlete
// profile and the metric store.
func mountAct(mux *http.ServeMux, app *App, logger *slog.Logger) {
	auth := restAuth(app)
	(&rest.ImportHandlers{Auth: auth, Imports: actImports(app, logger), Pipeline: app.Pipeline, Log: logger}).Mount(mux)
	(&rest.ActivitySourceHandlers{
		Auth: auth, Actions: app.SourceActions, Blobs: app.BlobStore,
		Reimporter: newSourceReimporter(app, logger), Log: logger,
	}).Mount(mux)
	if app.SourcePreviews != nil {
		(&rest.SourcePreviewHandlers{Auth: auth, Previews: app.SourcePreviews, Committer: &previewCommitter{app: app, logger: logger}}).Mount(mux)
	}
	(&rest.ActivityManageHandlers{Auth: auth, Actions: app.SourceActions, Details: app.Details, Pipeline: app.Pipeline, Log: logger}).Mount(mux)
	if app.FieldOverrides != nil {
		pins := &activity.FieldPins{Activities: app.Activities, Overrides: app.FieldOverrides, Remerge: app.Pipeline, Log: logger}
		(&rest.MergeOverrideHandlers{Auth: auth, Actions: app.SourceActions, Pins: pins, Links: app.SourceLinks, Log: logger}).Mount(mux)
	} else {
		logger.Info("merge-overrides api not mounted: field override repo not wired")
	}
	var policies *instance.MergePolicies
	if app.UserSettings != nil {
		policies = &instance.MergePolicies{Settings: app.UserSettings}
		(&rest.MergePolicyHandlers{Auth: auth, Policies: policies, Defaults: app.MergeDefaults, Log: logger}).Mount(mux)
	}
	if app.AthleteProfiles != nil {
		profile := &athlete.Profile{
			Repo: app.AthleteProfiles, Activities: app.Activities, Recompute: app.RecomputeActivity,
			TrainingLoad: app.ComputeTrainingLoad, Log: logger,
		}
		(&rest.AthleteHandlers{Auth: auth, Profile: profile}).Mount(mux)
	}
	if app.MetricStore != nil {
		(&rest.MetricHandlers{Auth: auth, Store: app.MetricStore, Policies: policies}).Mount(mux)
	}
	logger.Info("activity write endpoints mounted")
}

// actImports is the quota-gated import use case over the worker ingest path.
func actImports(app *App, logger *slog.Logger) *activity.Imports {
	imp := &activity.Imports{
		Ingest: &actIngester{app: app, logger: logger}, Gear: app.Gear,
		Activities: app.Activities, Recompute: app.RecomputeActivity, Log: logger,
	}
	if app.Quotas != nil {
		imp.Quota = app.QuotaViews
	}
	return imp
}

// uploadAPI is the Connect UploadActivity ingester; it shares the REST gate.
type uploadAPI struct {
	app    *App
	logger *slog.Logger
}

var _ port.UploadIngester = (*uploadAPI)(nil)

func (h *uploadAPI) IngestUpload(ctx context.Context, in port.UploadInput) (port.UploadResult, error) {
	return actImports(h.app, h.logger).IngestUpload(ctx, in)
}

// actIngester feeds user imports through ingestActivityEvent as a synthetic
// worker result (port.ImportIngester).
type actIngester struct {
	app    *App
	logger *slog.Logger
}

var _ port.ImportIngester = (*actIngester)(nil)

// IngestFile ingests under provider "upload" with the content hash as
// external id, so a re-upload dedups and refreshes; then archives the original.
func (g *actIngester) IngestFile(ctx context.Context, in port.UploadInput) (port.UploadResult, error) {
	payload, stream, err := fileimport.Parse(in.Filename, in.Raw)
	if err != nil {
		return port.UploadResult{}, port.NewActionError(port.ActionErrorInvalid, "parse failed: "+err.Error())
	}
	sum := sha256.Sum256(in.Raw)
	jr := &workerv1.JobResult{WorkerName: "file-upload", WorkerVersion: "1.0"}
	ev := &workerv1.ImportedActivity{
		Ref:     &workerv1.ExternalRef{UserId: in.UserID.String(), Provider: "upload", ExternalId: hex.EncodeToString(sum[:16])},
		Payload: payload,
		Stream:  stream,
	}
	if g.app.ActivityEvents != nil {
		g.app.ActivityEvents.Publish(domain.NewActivityEvent(domain.ActivityEventImportStarted, in.UserID, "activity.import_started").
			With("provider", "upload").With("filename", in.Filename))
	}
	out, err := ingestActivityEvent(ctx, g.app, g.logger, jr, ev)
	if err != nil {
		g.logger.Error("upload ingest failed", "user_id", in.UserID, "file", in.Filename, "error", err)
		return port.UploadResult{}, fmt.Errorf("ingest: %w", err)
	}
	g.archiveRaw(ctx, in, out.SourceID)
	return port.UploadResult{ActivityID: out.ActivityID, SourceID: out.SourceID, Action: string(out.Action)}, nil
}

// archiveRaw records the original file (best-effort: the activity is already
// ingested). A presigned upload already lives under in.RawBlobKey.
func (g *actIngester) archiveRaw(ctx context.Context, in port.UploadInput, sourceID domain.SourceID) {
	if g.app.BlobStore == nil {
		return
	}
	ext := fileimport.Ext(in.Filename)
	ct := fileimport.ContentType(ext)
	key := in.RawBlobKey
	if key == "" {
		key = fmt.Sprintf("raw/%s/%s%s", in.UserID.String(), sourceID.String(), ext)
		if err := g.app.BlobStore.Put(ctx, key, in.Raw, ct); err != nil {
			g.logger.Warn("archive raw upload failed", "source_id", sourceID, "error", err)
			return
		}
	}
	if err := g.app.Activities.SetSourceRawBlob(ctx, sourceID, key, ct, int64(len(in.Raw))); err != nil {
		g.logger.Warn("record raw blob ref failed", "source_id", sourceID, "error", err)
	}
}

func (g *actIngester) ValidateManual(in port.ManualActivity) error {
	if _, err := fileimport.Manual(in); err != nil {
		return port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	return nil
}

// IngestManual ingests under provider "manual" with a fresh external id, so
// each submission is a new activity.
func (g *actIngester) IngestManual(ctx context.Context, in port.ManualActivity) (port.UploadResult, error) {
	payload, err := fileimport.Manual(in)
	if err != nil {
		return port.UploadResult{}, port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	jr := &workerv1.JobResult{WorkerName: "manual-entry", WorkerVersion: "1.0"}
	ev := &workerv1.ImportedActivity{
		Ref:     &workerv1.ExternalRef{UserId: in.UserID.String(), Provider: "manual", ExternalId: uuid.NewString()},
		Payload: payload,
	}
	out, err := ingestActivityEvent(ctx, g.app, g.logger, jr, ev)
	if err != nil {
		return port.UploadResult{}, err
	}
	return port.UploadResult{ActivityID: out.ActivityID, SourceID: out.SourceID, Action: string(out.Action)}, nil
}
