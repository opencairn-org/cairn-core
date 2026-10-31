package activity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// previewActivityStore reads activities and flags source reimport status.
type previewActivityStore interface {
	port.ActivityReader
	port.SourceWriter
}

// SourcePreviews runs dry-run reimports: a pending preview claims the next
// worker result for its source, the result is diffed against the current
// source and merge instead of being ingested, and the owner then commits
// (the stored result is ingested through the normal path) or discards it.
type SourcePreviews struct {
	Repo       port.SourcePreviewRepo
	Actions    *SourceActions
	Recompute  *RecomputeActivityFromSources
	Reimporter port.SourceReimporter
	Activities previewActivityStore
	Events     port.ActivityEventSink
	Now        func() time.Time
}

func (p *SourcePreviews) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// Start creates a pending preview for the caller's source and dispatches the
// refetch (or local reparse) whose result it will capture.
func (p *SourcePreviews) Start(ctx context.Context, userID domain.UserID, activityID domain.ActivityID, sourceID domain.SourceID, preferLocal bool) (domain.SourcePreview, error) {
	if p.Reimporter == nil {
		return domain.SourcePreview{}, port.NewActionError(port.ActionErrorUnavailable, "re-import is not available on this instance")
	}
	_, src, err := p.Actions.OwnedSource(ctx, userID, activityID, sourceID)
	if err != nil {
		return domain.SourcePreview{}, err
	}
	if _, err := p.Repo.FindPendingForSource(ctx, sourceID, p.now()); err == nil {
		return domain.SourcePreview{}, port.NewActionError(port.ActionErrorConflict, "a preview for this source is already pending")
	}
	_, _ = p.Repo.DeleteExpired(ctx, p.now())
	var jobID string
	if preferLocal {
		jobID, err = p.Reimporter.Reparse(ctx, userID, src)
	} else {
		jobID, err = p.Reimporter.Refetch(ctx, userID, src)
	}
	if err != nil {
		return domain.SourcePreview{}, err
	}
	prev := domain.SourcePreview{
		SourceID: sourceID, ActivityID: activityID, UserID: userID, JobID: jobID,
		Status: domain.SourcePreviewPending, CreatedAt: p.now(), ExpiresAt: p.now().Add(domain.SourcePreviewTTL),
	}
	id, err := p.Repo.Create(ctx, prev)
	if err != nil {
		return domain.SourcePreview{}, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("store preview: %v", err))
	}
	prev.ID = id
	return prev, nil
}

// Capture stores a worker result for the source's pending preview, if any,
// with the diffs committing it would produce. claimed is false when no
// preview is waiting and the caller should ingest the result as usual.
func (p *SourcePreviews) Capture(ctx context.Context, current domain.ActivitySource, proposed domain.ActivitySourcePayload, raw []byte) (claimed bool, err error) {
	pending, err := p.Repo.FindPendingForSource(ctx, current.ID, p.now())
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find pending preview: %w", err)
	}
	candidate := current
	candidate.Parsed = proposed
	merged, err := p.Recompute.PreviewMerge(ctx, current.ActivityID, candidate)
	if err != nil {
		return true, fmt.Errorf("preview merge: %w", err)
	}
	before, err := p.Activities.GetActivity(ctx, current.ActivityID)
	if err != nil {
		return true, fmt.Errorf("get activity: %w", err)
	}
	sourceDiff := domain.DiffSourcePayloads(current.Parsed, proposed)
	mergedDiff := domain.DiffActivities(before, merged)
	if err := p.Repo.MarkReady(ctx, pending.ID, raw, sourceDiff, mergedDiff); err != nil {
		return true, fmt.Errorf("mark preview ready: %w", err)
	}
	// The dispatch flagged the source as updating; the preview holds the result instead.
	_ = p.Activities.SetSourceReimportStatus(ctx, current.ID, domain.ReimportStatusCurrent, "")
	if p.Events != nil {
		p.Events.Publish(domain.NewActivityEvent(domain.ActivityEventReimportCompleted, current.UserID, "activity.preview_ready").
			For(current.ActivityID, current.ID).With("preview_id", pending.ID.String()).
			With("changes", fmt.Sprint(len(sourceDiff))))
	}
	return true, nil
}

// Get returns the caller's preview; expired ones read as not found.
func (p *SourcePreviews) Get(ctx context.Context, userID domain.UserID, id domain.SourcePreviewID) (domain.SourcePreview, error) {
	prev, err := p.Repo.Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && prev.UserID != userID) || (err == nil && prev.Expired(p.now())) {
		return domain.SourcePreview{}, port.NewActionError(port.ActionErrorNotFound, "preview not found or expired")
	}
	if err != nil {
		return domain.SourcePreview{}, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("load preview: %v", err))
	}
	return prev, nil
}

// TakeForCommit returns a ready preview's stored result and removes the
// preview; the caller ingests the result. Pending previews cannot be committed.
func (p *SourcePreviews) TakeForCommit(ctx context.Context, userID domain.UserID, id domain.SourcePreviewID) (domain.SourcePreview, error) {
	prev, err := p.Get(ctx, userID, id)
	if err != nil {
		return domain.SourcePreview{}, err
	}
	if prev.Status != domain.SourcePreviewReady {
		return domain.SourcePreview{}, port.NewActionError(port.ActionErrorPrecondition, "the preview is still waiting for the worker result")
	}
	if err := p.Repo.Delete(ctx, userID, id); err != nil {
		return domain.SourcePreview{}, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("consume preview: %v", err))
	}
	return prev, nil
}

// Discard removes a preview without applying it.
func (p *SourcePreviews) Discard(ctx context.Context, userID domain.UserID, id domain.SourcePreviewID) error {
	if _, err := p.Get(ctx, userID, id); err != nil {
		return err
	}
	if err := p.Repo.Delete(ctx, userID, id); err != nil {
		return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("delete preview: %v", err))
	}
	return nil
}
