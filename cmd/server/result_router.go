package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/encoding/protojson"

	workerv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/worker/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/domain/health"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/protoconv"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	"github.com/opencairn-org/cairn-core/internal/usecase/segment"
)

// startResultRouter subscribes to cairn.results.fetch_source.> and feeds
// worker-published results through the post-ingest pipeline.
//
// The wire format is the typed cairn.worker.v1.JobResult, protojson-encoded
// — there is no hand-rolled JSON envelope. A JobResult carries an ordered
// list of typed WorkerEvents (segments → activity → efforts → records);
// each is routed to its ingest path.
//
// Gating: returns (nil, nil) if the NATSBus isn't wired (single-process mode).
func startResultRouter(
	ctx context.Context,
	app *App,
	logger *slog.Logger,
) (port.Subscription, error) {
	if app.NATSBus == nil {
		return nil, nil
	}

	log := logger.With("component", "result_router")
	// fetch_source and parse_blob produce the SAME JobResult wire format; the
	// router doesn't care which path produced it (parse_blob re-parses an
	// archived blob, fetch_source pulls fresh). The CAIRN_RESULTS stream only
	// carries these JobResults, so one consumer over cairn.results.> routes both.
	log.Info("subscribing to cairn.results.>")

	return app.NATSBus.Subscribe(ctx, port.ConsumerConfig{
		Stream:        "CAIRN_RESULTS",
		Durable:       "ingest-router",
		Subject:       "cairn.results.>",
		DeliverPolicy: port.DeliverAll,
	}, func(ctx context.Context, msg port.Message) error {
		return routeJobResult(ctx, app, log, msg)
	})
}

// resultUnmarshal tolerates fields this core does not know, so a newer
// worker's additive contract changes never fail every import on an older core.
var resultUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// routeJobResult decodes the typed JobResult and processes its events in
// order. Extracted so it's unit-testable independent of the NATS glue.
func routeJobResult(ctx context.Context, app *App, log *slog.Logger, msg port.Message) error {
	var jr workerv1.JobResult
	if err := resultUnmarshal.Unmarshal(msg.Body, &jr); err != nil {
		log.Warn("decode JobResult failed", "subject", msg.Subject, "error", err)
		return &port.TerminalError{Reason: "bad_payload", Cause: err}
	}

	// Worker-reported terminal failure: fail the queue item now with the
	// true reason. Without this the item sits in_progress until the stale
	// reaper masks the cause as "no result after N dispatch attempts".
	if err := checkResultPool(ctx, app, log, msg.Subject, &jr); err != nil {
		log.Error("result rejected", "subject", msg.Subject, "worker", jr.GetWorkerName(), "package", jr.GetWorkerPackage(), "error", err)
		return nil // deterministic: redelivery cannot fix it
	}
	if jr.GetError() != nil {
		applyWorkerFailure(ctx, app, log, msg.Subject, &jr)
		return nil
	}

	// Claim-check: the envelope points at the full JobResult in the blob
	// store. Fetch + decode it, process, and delete only after success —
	// a NAK'd redelivery must still find the object (lifecycle rule reaps
	// orphans that never process).
	if ref := jr.GetPayloadRef(); ref != nil {
		return routeClaimCheckedResult(ctx, app, log, msg.Subject, ref)
	}

	return processJobResult(ctx, app, log, msg.Subject, &jr)
}

// routeClaimCheckedResult fetches, processes and then deletes a claim-checked JobResult.
func routeClaimCheckedResult(ctx context.Context, app *App, log *slog.Logger, subject string, ref *workerv1.PayloadRef) error {
	if app.BlobStore == nil {
		log.Error("claim-checked result but no blob store configured", "subject", subject, "blob_id", ref.GetBlobId())
		return &port.TerminalError{Reason: "no_blob_store", Cause: errors.New("payload_ref without blob store")}
	}
	data, _, err := app.BlobStore.Get(ctx, ref.GetBlobId())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Expired (envelope older than the lifecycle backstop) or
			// already consumed by a duplicate delivery. Not retryable.
			log.Error("claim-checked payload missing", "subject", subject, "blob_id", ref.GetBlobId())
			return &port.TerminalError{Reason: "payload_missing", Cause: err}
		}
		return fmt.Errorf("fetch claim-checked payload %s: %w", ref.GetBlobId(), err)
	}
	var full workerv1.JobResult
	if err := resultUnmarshal.Unmarshal(data, &full); err != nil {
		log.Warn("decode claim-checked JobResult failed", "subject", subject, "blob_id", ref.GetBlobId(), "error", err)
		return &port.TerminalError{Reason: "bad_payload", Cause: err}
	}
	if err := processJobResult(ctx, app, log, subject, &full); err != nil {
		return err
	}
	if err := app.BlobStore.Delete(ctx, ref.GetBlobId()); err != nil {
		log.Warn("delete claim-checked payload failed (lifecycle rule will reap)", "blob_id", ref.GetBlobId(), "error", err)
	}
	return nil
}

// processJobResult routes a fully-materialized JobResult (inline or
// resolved from a claim-check ref) to the ingest paths.
func processJobResult(ctx context.Context, app *App, log *slog.Logger, subject string, jr *workerv1.JobResult) error {
	// Reconcile results carry no events — their payload is the watermark.
	// Applying it (and last_sync_at) even when nothing new was found is what
	// keeps the account from being perpetually "due": without it the scheduler
	// re-polls every tick and the provider budget drains into list calls.
	if applyReconcileResult(ctx, app, log, subject, jr) {
		return nil
	}

	// Activities and segments/efforts arrive as separate JobResult messages (the
	// worker splits them so a segment-heavy activity's stream doesn't blow the
	// NATS payload limit). Within a segment message, segments come before
	// efforts; the bundle's segment batch lets an effort resolve a segment
	// created moments earlier without a DB round-trip. Each effort resolves its activity by
	// external id (FindSourceByExternalID), so the segment message is
	// self-contained.
	job, _, _, _ := domain.ResultSubjectPool(subject)
	b := &ingestBundle{job: job}

	for _, ev := range jr.GetEvents() {
		if err := processWorkerEvent(ctx, app, log, jr, b, ev); err != nil {
			return err
		}
	}
	return nil
}

// processWorkerEvent ingests one event; a returned error NAKs the message.
func processWorkerEvent(ctx context.Context, app *App, log *slog.Logger, jr *workerv1.JobResult, b *ingestBundle, ev *workerv1.WorkerEvent) error {
	switch ev.GetType() {
	case workerv1.WorkerEventType_WORKER_EVENT_TYPE_ACTIVITY:
		return processActivityEvent(ctx, app, log, jr, b, ev.GetActivity())

	case workerv1.WorkerEventType_WORKER_EVENT_TYPE_SEGMENT:
		// Non-fatal: a bad segment must not fail the durable activity ingest.
		if err := ingestSegmentEvent(ctx, app, b, ev.GetSegment()); err != nil {
			log.Warn("segment ingest failed", "error", err)
		}

	case workerv1.WorkerEventType_WORKER_EVENT_TYPE_SEGMENT_EFFORT:
		if err := ingestSegmentEffortEvent(ctx, app, log, b, ev.GetSegmentEffort()); err != nil {
			log.Warn("segment effort ingest failed", "error", err)
		}

	case workerv1.WorkerEventType_WORKER_EVENT_TYPE_METRIC:
		// Non-fatal: a health sample must not fail the batch.
		if err := ingestMetricEvent(ctx, app, ev.GetMetric()); err != nil {
			log.Warn("metric ingest failed", "error", err)
		}

	case workerv1.WorkerEventType_WORKER_EVENT_TYPE_GEAR:
		// Non-fatal: gear is optional data; the activity that references it
		// resolves the ref later (next import_gear run or activity).
		if err := ingestGearEvent(ctx, app, ev.GetGear()); err != nil {
			log.Warn("gear ingest failed", "error", err)
		}

	case workerv1.WorkerEventType_WORKER_EVENT_TYPE_RECORD:
		// Provider records ingestion is a separate milestone; Cairn computes
		// canonical best-efforts itself from streams.
		log.Info("entity ingest pending for event type", "type", ev.GetType().String())
	default:
		log.Warn("unknown worker event type", "type", ev.GetType().String())
	}
	return nil
}

// applyReconcileResult handles a reconcile job's result, identified by its
// subject shape cairn.results.reconcile.<provider>.<external_account_id>.
// It advances the account's sync watermark AND last_sync_at — the latter is
// what ListAccountsDueForReconcile keys on, so a reconcile that found nothing
// new still counts as "checked" and the account isn't re-polled every tick.
// Returns whether the message was a reconcile result (and thus fully handled).
func applyReconcileResult(ctx context.Context, app *App, log *slog.Logger, subject string, jr *workerv1.JobResult) bool {
	// cairn.results.reconcile.<provider>[.<pool>].<account>
	parts := strings.Split(subject, ".")
	if (len(parts) != 5 && len(parts) != 6) || parts[0] != "cairn" || parts[1] != "results" || parts[2] != "reconcile" {
		return false
	}
	if app.ExternalAccounts == nil {
		return true
	}
	acctToken := parts[len(parts)-1]
	au, err := uuid.Parse(acctToken)
	if err != nil {
		log.Warn("reconcile result with unparseable account id", "subject", subject)
		return true
	}
	// A zero watermark (account with no activities yet) is filtered to NULL
	// by the repo, so it can't regress or pollute the stored watermark.
	var wm time.Time
	if ts := jr.GetNewWatermark(); ts != nil {
		wm = ts.AsTime()
	}
	if err := app.ExternalAccounts.UpdateSyncWatermark(ctx, domain.ExternalAccountID(au), wm, true, time.Now().UTC()); err != nil {
		log.Warn("apply reconcile result failed", "account_id", acctToken, "error", err)
	}
	return true
}

// applyWorkerFailure handles a JobResult that carries a worker-side
// terminal error instead of events. failed_ref correlates it to the
// import-queue item; results without one (reconcile, backfill) just log.
func applyWorkerFailure(ctx context.Context, app *App, log *slog.Logger, subject string, jr *workerv1.JobResult) {
	we := jr.GetError()
	ref := jr.GetFailedRef()
	log.Warn("worker reported terminal job failure",
		"subject", subject,
		"class", we.GetClass().String(),
		"code", we.GetCode(),
		"error", we.GetMessage(),
		"external_id", ref.GetExternalId())
	if app.ImportQueue == nil || ref.GetExternalAccountId() == "" || ref.GetExternalId() == "" {
		return
	}
	eu, err := uuid.Parse(ref.GetExternalAccountId())
	if err != nil {
		log.Warn("worker error ref has malformed external_account_id", "value", ref.GetExternalAccountId(), "error", err)
		return
	}
	reason := "worker: " + we.GetCode()
	if m := we.GetMessage(); m != "" {
		reason += ": " + m
	}
	if job, provider, _, ok := domain.ResultSubjectPool(subject); ok && job == "parse_blob" {
		markReparseFailed(ctx, app, log, provider, ref, we.GetCode(), reason)
		publishFailure(app, ref, domain.ActivityEventReimportFailed, "activity.reimport_failed", reason)
		return
	}
	if err := app.ImportQueue.MarkFailedByExternalID(ctx, domain.ExternalAccountID(eu), domain.ImportItemActivity, ref.GetExternalId(), reason); err != nil {
		log.Warn("mark queue item failed", "external_id", ref.GetExternalId(), "error", err)
	}
	publishFailure(app, ref, domain.ActivityEventImportFailed, "activity.import_failed", reason)
}

// publishFailure emits a live failure event for the user named in the ref.
func publishFailure(app *App, ref *workerv1.ExternalRef, t domain.ActivityEventType, key, reason string) {
	if app.ActivityEvents == nil {
		return
	}
	uid, err := uuid.Parse(ref.GetUserId())
	if err != nil {
		return
	}
	app.ActivityEvents.Publish(domain.NewActivityEvent(t, domain.UserID(uid), key).
		With("provider", ref.GetProvider()).With("external_id", ref.GetExternalId()).With("reason", reason))
}

// publishIngestOutcome emits the live events of a successful ingest: the
// import or reimport completion plus a created event for a new activity.
func publishIngestOutcome(app *App, provider string, out activity.IngestResult) {
	if app.ActivityEvents == nil {
		return
	}
	uid := out.Activity.UserID
	done := domain.NewActivityEvent(domain.ActivityEventImportCompleted, uid, "activity.import_completed")
	if out.Action == activity.IngestActionReimportedExisting {
		done = domain.NewActivityEvent(domain.ActivityEventReimportCompleted, uid, "activity.reimport_completed")
	}
	app.ActivityEvents.Publish(done.For(out.ActivityID, out.SourceID).With("provider", provider).With("title", out.Activity.Title))
	if out.Action == activity.IngestActionImportedNew {
		app.ActivityEvents.Publish(domain.NewActivityEvent(domain.ActivityEventActivityCreated, uid, "activity.created").
			For(out.ActivityID, out.SourceID).With("title", out.Activity.Title))
	}
}

// markReparseFailed records a failed re-parse on the source. parser_too_old
// (no instance of the pool is new enough yet) keeps the source flagged for
// re-parse instead of failing it.
func markReparseFailed(ctx context.Context, app *App, log *slog.Logger, provider string, ref *workerv1.ExternalRef, code, reason string) {
	acctUUID, err := uuid.Parse(ref.GetExternalAccountId())
	if err != nil {
		return
	}
	acct := domain.ExternalAccountID(acctUUID)
	src, err := app.Activities.FindSourceByExternalID(ctx, provider, &acct, ref.GetExternalId())
	if err != nil {
		log.Warn("re-parse failure for unknown source", "external_id", ref.GetExternalId(), "error", err)
		return
	}
	status, why := domain.ReimportStatusFailed, reason
	if code == "parser_too_old" {
		status, why = domain.ReimportStatusUpdateAvailable, domain.ReimportReasonNewerParser
	}
	if err := app.Activities.SetSourceReimportStatus(ctx, src.ID, status, why); err != nil {
		log.Warn("set reimport status after failure", "source_id", src.ID, "error", err)
	}
}

// isDeterministicDBError reports whether err is a Postgres integrity (class
// 23) or data (class 22) violation — deterministic for a given payload, so
// redelivering the same result can never succeed. Locking/timeout classes
// (40, 55, 57) stay transient on purpose: those DO resolve on retry.
func isDeterministicDBError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return strings.HasPrefix(pgErr.Code, "23") || strings.HasPrefix(pgErr.Code, "22")
}

// failQueueItem marks the import-queue item failed after a terminal ingest
// rejection, so the per-account queue advances instead of stalling.
func failQueueItem(ctx context.Context, app *App, log *slog.Logger, ref *workerv1.ExternalRef, cause error) {
	log.Warn("activity ingest rejected (terminal)", "external_id", ref.GetExternalId(), "error", cause)
	if app.ImportQueue == nil || ref.GetExternalAccountId() == "" {
		return
	}
	eu, err := uuid.Parse(ref.GetExternalAccountId())
	if err != nil {
		log.Warn("ingest ref has malformed external_account_id", "value", ref.GetExternalAccountId(), "error", err)
		return
	}
	if err := app.ImportQueue.MarkFailedByExternalID(ctx, domain.ExternalAccountID(eu), domain.ImportItemActivity, ref.GetExternalId(), "invalid: "+cause.Error()); err != nil {
		log.Warn("mark queue item failed", "external_id", ref.GetExternalId(), "error", err)
	}
}

// ingestMetricEvent persists a non-activity health metric (HRV, Sleep, Weight,
// Steps, RestingHR) into the health_samples store, keyed by its canonical
// DataType (the metric key). Skips unknown/non-health keys.
func ingestMetricEvent(ctx context.Context, app *App, im *workerv1.ImportedMetric) error {
	if app.Health == nil || im == nil || im.ValueNumeric == nil {
		return nil
	}
	dt := capability.DataType(im.GetKey())
	if dt.CategoryOf() != capability.CategoryTimeSeries {
		return nil // not a health metric
	}
	ref := im.GetRef()
	uid, err := uuid.Parse(ref.GetUserId())
	if err != nil {
		return fmt.Errorf("metric: bad user id: %w", err)
	}
	var acct *domain.ExternalAccountID
	if s := ref.GetExternalAccountId(); s != "" {
		if eu, err := uuid.Parse(s); err == nil {
			ea := domain.ExternalAccountID(eu)
			acct = &ea
		}
	}
	v := im.GetValueNumeric()
	return app.Health.SaveSamples(ctx, domain.UserID(uid), acct, []health.Sample{{
		UserID:    domain.UserID(uid),
		DataType:  dt,
		Timestamp: im.GetTs().AsTime(),
		Provider:  ref.GetProvider(),
		Value:     &v,
	}})
}

// ingestGearEvent records a provider's gear record (docs/gear-v0.4.0.md,
// contract §5). The use case derives the owner from the external account
// and rejects a ref whose user_id or provider disagrees with it.
func ingestGearEvent(ctx context.Context, app *App, g *workerv1.ImportedGear) error {
	if app.Gear == nil || g == nil {
		return nil
	}
	ref := g.GetRef()
	if ref.GetExternalId() == "" {
		return errors.New("gear event missing external_id")
	}
	uid, err := uuid.Parse(ref.GetUserId())
	if err != nil {
		return fmt.Errorf("gear event bad user_id: %w", err)
	}
	accUUID, err := uuid.Parse(ref.GetExternalAccountId())
	if err != nil {
		return fmt.Errorf("gear event bad external_account_id: %w", err)
	}
	key := domain.GearRefKey{
		Provider:          ref.GetProvider(),
		ExternalAccountID: domain.ExternalAccountID(accUUID),
		ExternalID:        ref.GetExternalId(),
	}
	_, err = app.Gear.ImportFromProvider(ctx, domain.UserID(uid), key, protoconv.GearImportFromProto(g.GetPayload()))
	return err
}

// processActivityEvent ingests one activity; a returned error NAKs the message.
func processActivityEvent(ctx context.Context, app *App, log *slog.Logger, jr *workerv1.JobResult, b *ingestBundle, ia *workerv1.ImportedActivity) error {
	// A pool may only import for its own accounts and re-parse its own
	// package's sources (docs/adr/0006).
	if err := checkResultAccount(ctx, app, b.job, jr, ia); err != nil {
		failQueueItem(ctx, app, log, ia.GetRef(), err)
		return nil
	}
	if capturePreview(ctx, app, log, b.job, jr, ia) {
		return nil
	}
	_, err := ingestActivityEvent(ctx, app, log, jr, ia)
	if err == nil {
		return nil
	}
	// Terminal (bad data) → fail the queue item + ACK; retrying the same
	// payload can't help, and leaving it in_progress would deadlock the
	// account's capacity-gated queue. Postgres integrity/data violations are
	// equally deterministic for a given payload — without this they NAK-loop
	// into the DLQ and the item wedges as a masked "stale: no result" (live
	// case: duplicate stream timestamps, 5 wasted provider re-fetches per
	// activity).
	if domain.IsActivityValidationError(err) || isDeterministicDBError(err) {
		failQueueItem(ctx, app, log, ia.GetRef(), err)
		return nil
	}
	// Transient (DB blip) → NAK; JetStream redelivers. Re-processing is
	// safe — ingest dedups on (provider, external_id).
	return err
}

// ingestBundle carries per-message state: the segment batch memoises
// segments upserted earlier in the same JobResult.
type ingestBundle struct {
	segments *segment.ProviderSegmentBatch
	job      string // job kind of the result subject
}

func (b *ingestBundle) segmentBatch(app *App) *segment.ProviderSegmentBatch {
	if b.segments == nil {
		b.segments = app.ProviderSegments.Batch()
	}
	return b.segments
}

// ingestSegmentEvent decodes a provider segment and upserts it.
func ingestSegmentEvent(ctx context.Context, app *App, b *ingestBundle, seg *workerv1.ImportedSegment) error {
	if app.ProviderSegments == nil || seg == nil {
		return nil
	}
	ref := seg.GetRef()
	accUUID, err := uuid.Parse(ref.GetExternalAccountId())
	if err != nil {
		return fmt.Errorf("segment event bad external_account_id: %w", err)
	}
	s := protoconv.SegmentFromProto(seg.GetPayload(), ref.GetExternalId(), domain.ExternalAccountID(accUUID))
	_, err = b.segmentBatch(app).Upsert(ctx, s)
	return err
}

// ingestSegmentEffortEvent decodes a provider effort and records it under the
// canonical-effort rule (segment.ProviderSegmentBatch.RecordEffort).
func ingestSegmentEffortEvent(ctx context.Context, app *App, log *slog.Logger, b *ingestBundle, eff *workerv1.ImportedSegmentEffort) error {
	if app.ProviderSegments == nil || eff == nil {
		return nil
	}
	ref := eff.GetRef()
	accUUID, err := uuid.Parse(ref.GetExternalAccountId())
	if err != nil {
		return fmt.Errorf("segment effort bad external_account_id: %w", err)
	}
	res, err := b.segmentBatch(app).RecordEffort(ctx, segment.ProviderEffort{
		Provider:           ref.GetProvider(),
		AccountID:          domain.ExternalAccountID(accUUID),
		SegmentExternalID:  eff.GetSegmentExternalId(),
		ActivityExternalID: eff.GetActivityExternalId(),
		ProviderEffortID:   ref.GetExternalId(),
		Effort:             protoconv.SegmentEffortFromProto(eff.GetPayload(), ref.GetExternalId()),
	})
	if err != nil {
		return err
	}
	if res.Outcome == segment.EffortDropped {
		// Segment geometry gap or tolerance miss; the matcher stays authoritative.
		log.Info("provider effort has no matching matcher effort; dropped",
			"segment_id", res.SegmentID, "source_id", res.SourceID,
			"provider_effort", ref.GetExternalId(), "start_time", eff.GetPayload().GetStartTime().AsTime())
	}
	return nil
}

// ingestActivityEvent maps a typed ImportedActivity to the domain IngestInput
// and runs it through ingest + the post-ingest follow-ups.
func ingestActivityEvent(
	ctx context.Context,
	app *App,
	log *slog.Logger,
	jr *workerv1.JobResult,
	ia *workerv1.ImportedActivity,
) (activity.IngestResult, error) {
	if ia == nil {
		return activity.IngestResult{}, &port.TerminalError{Reason: "missing_activity"}
	}
	ref := ia.GetRef()

	uid, extAcct, err := ingestActivityParseRef(ref)
	if err != nil {
		return activity.IngestResult{}, err
	}

	payload := protoconv.ActivitySourcePayloadFromProto(ia.GetPayload())
	stream := protoconv.ActivityStreamSamplesFromProto(ia.GetStream(), payload.StartTime)
	payload.HasStream = len(stream) > 0

	in := activity.IngestInput{
		UserID:              domain.UserID(uid),
		Provider:            ref.GetProvider(),
		ExternalAccountID:   extAcct,
		ExternalID:          ref.GetExternalId(),
		SourceWorkerName:    jr.GetWorkerName(),
		SourceWorkerVersion: jr.GetWorkerVersion(),
		SourceWorkerPackage: jr.GetWorkerPackage(),
		SourceWorkerRelease: jr.GetWorkerRelease(),
		RawBlobID:           ia.GetRawBlobId(),
		RawContentType:      ia.GetRawContentType(),
		RawSizeBytes:        ia.GetRawSizeBytes(),
		Payload:             payload,
		Stream:              stream,
	}

	// Persist, re-cluster and follow-ups (pipeline.IngestSource); this is the
	// only ingest path. What remains here is wire-format bookkeeping.
	out, err := app.Pipeline.IngestSource(ctx, in)
	if err != nil {
		log.Warn("ingest failed",
			"provider", in.Provider, "external_id", in.ExternalID, "error", err)
		return activity.IngestResult{}, err
	}
	ingestActivityGearRef(ctx, app, log, in)
	log.Info("ingest succeeded",
		"activity_id", out.ActivityID, "source_id", out.SourceID, "action", out.Action)
	publishIngestOutcome(app, in.Provider, out)

	ingestActivityAttachments(ctx, app, log, ia, out, in.UserID)
	if extAcct != nil {
		ingestActivityAccountBookkeeping(ctx, app, log, *extAcct, ref.GetExternalId(), payload.StartTime, out)
	}
	return out, nil
}

// ingestActivityParseRef parses the user and optional external-account ids of an activity ref.
func ingestActivityParseRef(ref *workerv1.ExternalRef) (uuid.UUID, *domain.ExternalAccountID, error) {
	uid, err := uuid.Parse(ref.GetUserId())
	if err != nil {
		return uuid.UUID{}, nil, &port.TerminalError{Reason: "bad_user_id", Cause: err}
	}
	s := ref.GetExternalAccountId()
	if s == "" {
		return uid, nil, nil
	}
	eu, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, nil, &port.TerminalError{Reason: "bad_external_account_id", Cause: err}
	}
	ea := domain.ExternalAccountID(eu)
	return uid, &ea, nil
}

// ingestActivityGearRef makes an unknown provider gear id a pending decision.
// Non-fatal, like the gear event.
func ingestActivityGearRef(ctx context.Context, app *App, log *slog.Logger, in activity.IngestInput) {
	if app.Gear == nil || in.ExternalAccountID == nil || in.Payload.GearExternalID == "" {
		return
	}
	key := domain.GearRefKey{Provider: in.Provider, ExternalAccountID: *in.ExternalAccountID, ExternalID: in.Payload.GearExternalID}
	if err := app.Gear.NoteActivityGear(ctx, in.UserID, key, in.Payload.GearName); err != nil {
		log.Warn("note activity gear failed", "provider", in.Provider, "gear_external_id", key.ExternalID, "error", err)
	}
}

// ingestActivityAttachments persists attachments the worker mirrored into blob
// storage. Replace-by-source so a reimport refreshes exactly this source's set.
// Best-effort: a failure here must not fail the durable ingest.
func ingestActivityAttachments(ctx context.Context, app *App, log *slog.Logger, ia *workerv1.ImportedActivity, out activity.IngestResult, userID domain.UserID) {
	if app.Attachments == nil || len(ia.GetAttachments()) == 0 {
		return
	}
	atts := make([]domain.Attachment, 0, len(ia.GetAttachments()))
	for _, a := range ia.GetAttachments() {
		if a.GetBlobId() == "" {
			continue
		}
		atts = append(atts, domain.Attachment{
			ActivityID:  out.ActivityID,
			UserID:      userID,
			BlobID:      a.GetBlobId(),
			ExternalURL: a.GetExternalUrl(),
			ContentType: a.GetContentType(),
			Caption:     a.GetCaption(),
			Width:       int(a.GetWidth()),
			Height:      int(a.GetHeight()),
		})
	}
	if err := app.Attachments.ReplaceForSource(ctx, out.ActivityID, out.SourceID, atts); err != nil {
		log.Warn("persist attachments failed", "source_id", out.SourceID, "error", err)
		return
	}
	// The worker stored the originals; derive the display variants here.
	if app.AttachmentVariants != nil {
		app.AttachmentVariants.EnsureForActivity(ctx, out.ActivityID)
	}
}

// ingestActivityAccountBookkeeping updates the queue item, sync watermark and import history of the account.
func ingestActivityAccountBookkeeping(
	ctx context.Context,
	app *App,
	log *slog.Logger,
	extAcct domain.ExternalAccountID,
	externalID string,
	startTime time.Time,
	out activity.IngestResult,
) {
	// Mark the matching import-queue item done (correlate by the dedup key).
	if app.ImportQueue != nil {
		if err := app.ImportQueue.MarkDoneByExternalID(ctx, extAcct, domain.ImportItemActivity, externalID); err != nil {
			log.Warn("mark queue item done failed", "external_id", externalID, "error", err)
		}
	}

	// Advance the account's sync watermark to this activity's start time
	// (GREATEST in SQL — only moves forward). This keeps the reconcile window
	// bounded to genuinely-recent activity so the auto-reconciler never
	// re-lists the full history. Full backfill is the user-driven import
	// queue's job; reconcile only catches new activity since the watermark.
	if app.ExternalAccounts != nil && !startTime.IsZero() {
		if err := app.ExternalAccounts.UpdateSyncWatermark(ctx, extAcct, startTime, true, time.Now().UTC()); err != nil {
			log.Warn("advance sync watermark failed", "account_id", extAcct, "error", err)
		}
	}

	ingestActivityRecordEvent(ctx, app, log, extAcct, externalID, out)
}

// ingestActivityRecordEvent appends to the connection's import history (best-effort).
// Captures every import path — full sync, reconcile, webhook — since they all land here.
func ingestActivityRecordEvent(ctx context.Context, app *App, log *slog.Logger, extAcct domain.ExternalAccountID, externalID string, out activity.IngestResult) {
	if app.ImportEvents == nil {
		return
	}
	kind := domain.ImportEventActivityImported
	if out.Action == activity.IngestActionReimportedExisting || out.Action == activity.IngestActionMergedIntoExisting {
		kind = domain.ImportEventActivityUpdated
	}
	title := out.Activity.Title
	if title == "" {
		title = externalID
	}
	if err := app.ImportEvents.Record(ctx, domain.ConnectionImportEvent{
		ExternalAccountID: extAcct,
		Kind:              kind,
		Detail:            title,
		ExternalID:        externalID,
	}); err != nil {
		log.Warn("record import event failed", "account_id", extAcct, "error", err)
	}
}
