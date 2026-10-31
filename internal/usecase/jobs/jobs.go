// Package jobs is the admin view of core's work: the per-account import
// queue (the closest thing to a job registry) and the dead-letter queue.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

// Replayer republishes a dead-lettered payload (nats.DeadLetterReplayer).
type Replayer interface {
	Replay(ctx context.Context, job domain.DeadLetteredJob) (newMsgID string, err error)
}

// Jobs lists and steers queue items and dead letters.
type Jobs struct {
	Queue       port.ImportQueueAdmin
	DeadLetters port.DeadLetterRepo
	Replay      Replayer
	Audit       *audit.Recorder
	Now         func() time.Time
}

func (j *Jobs) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now().UTC()
}

func internalErr(op string, err error) error {
	return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("%s: %v", op, err))
}

// List pages queue items with the filter plus the per-status counts.
func (j *Jobs) List(ctx context.Context, f port.ImportQueueFilter) ([]domain.ImportQueueItem, int, map[domain.ImportItemStatus]int, error) {
	if j.Queue == nil {
		return nil, 0, nil, port.NewActionError(port.ActionErrorUnavailable, "the import queue is not available")
	}
	items, total, err := j.Queue.ListAll(ctx, f)
	if err != nil {
		return nil, 0, nil, internalErr("list jobs", err)
	}
	counts, err := j.Queue.CountAllByStatus(ctx, port.ImportQueueFilter{Providers: f.Providers, UserID: f.UserID, ItemTypes: f.ItemTypes, From: f.From, To: f.To})
	if err != nil {
		return nil, 0, nil, internalErr("count jobs", err)
	}
	return items, total, counts, nil
}

// Get returns one queue item.
func (j *Jobs) Get(ctx context.Context, id domain.ImportQueueItemID) (domain.ImportQueueItem, error) {
	if j.Queue == nil {
		return domain.ImportQueueItem{}, port.NewActionError(port.ActionErrorUnavailable, "the import queue is not available")
	}
	it, err := j.Queue.Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ImportQueueItem{}, port.NewActionError(port.ActionErrorNotFound, "job not found")
	}
	if err != nil {
		return domain.ImportQueueItem{}, internalErr("load job", err)
	}
	return it, nil
}

// Retry puts a failed or skipped item back in line.
func (j *Jobs) Retry(ctx context.Context, actor audit.Actor, id domain.ImportQueueItemID, reset bool) (domain.ImportQueueItem, error) {
	if _, err := j.Get(ctx, id); err != nil {
		return domain.ImportQueueItem{}, err
	}
	ok, err := j.Queue.Retry(ctx, id, reset)
	if err != nil {
		return domain.ImportQueueItem{}, internalErr("retry job", err)
	}
	if !ok {
		return domain.ImportQueueItem{}, port.NewActionError(port.ActionErrorPrecondition, "only failed or cancelled jobs can be retried")
	}
	j.Audit.Info(ctx, actor, domain.AuditJobRetried, "job", id.String(), map[string]string{"reset_attempts": fmt.Sprint(reset)})
	return j.Get(ctx, id)
}

// Cancel skips a pending or running item.
func (j *Jobs) Cancel(ctx context.Context, actor audit.Actor, id domain.ImportQueueItemID) (domain.ImportQueueItem, error) {
	if _, err := j.Get(ctx, id); err != nil {
		return domain.ImportQueueItem{}, err
	}
	ok, err := j.Queue.Cancel(ctx, id, "cancelled by admin")
	if err != nil {
		return domain.ImportQueueItem{}, internalErr("cancel job", err)
	}
	if !ok {
		return domain.ImportQueueItem{}, port.NewActionError(port.ActionErrorPrecondition, "only pending or running jobs can be cancelled")
	}
	j.Audit.Info(ctx, actor, domain.AuditJobCancelled, "job", id.String(), nil)
	return j.Get(ctx, id)
}

// ListDeadLetters pages the dead-letter queue.
func (j *Jobs) ListDeadLetters(ctx context.Context, f port.DeadLetterFilter) ([]domain.DeadLetteredJob, error) {
	if j.DeadLetters == nil {
		return nil, port.NewActionError(port.ActionErrorUnavailable, "the dead-letter queue is not available")
	}
	list, err := j.DeadLetters.List(ctx, f)
	if err != nil {
		return nil, internalErr("list dead letters", err)
	}
	return list, nil
}

// Requeue republishes a dead-lettered payload and marks the row replayed.
func (j *Jobs) Requeue(ctx context.Context, actor audit.Actor, id domain.DeadLetteredJobID) (domain.DeadLetteredJob, string, error) {
	if j.DeadLetters == nil || j.Replay == nil {
		return domain.DeadLetteredJob{}, "", port.NewActionError(port.ActionErrorUnavailable, "dead-letter replay is not available")
	}
	job, err := j.DeadLetters.Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.DeadLetteredJob{}, "", port.NewActionError(port.ActionErrorNotFound, "dead letter not found")
	}
	if err != nil {
		return domain.DeadLetteredJob{}, "", internalErr("load dead letter", err)
	}
	if len(job.Payload) == 0 {
		return domain.DeadLetteredJob{}, "", port.NewActionError(port.ActionErrorPrecondition, "the captured row has no payload to replay")
	}
	msgID, err := j.Replay.Replay(ctx, job)
	if err != nil {
		return domain.DeadLetteredJob{}, "", port.NewActionError(port.ActionErrorUpstream, "replay failed: "+err.Error())
	}
	if err := j.DeadLetters.MarkReplayed(ctx, id, actor.UserID, j.now()); err != nil {
		return domain.DeadLetteredJob{}, msgID, internalErr("mark replayed", err)
	}
	j.Audit.Info(ctx, actor, domain.AuditJobRequeued, "dead_letter", id.String(), map[string]string{"new_msg_id": msgID})
	job, _ = j.DeadLetters.Get(ctx, id)
	return job, msgID, nil
}
