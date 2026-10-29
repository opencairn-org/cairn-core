// Package maintenance runs the instance-wide recomputations an admin
// triggers (TriggerMaintenance): one job at a time in the background, with
// an in-memory registry for progress and cancellation. Jobs do not survive
// a restart; the work itself is idempotent, so re-triggering is safe.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

// Per-id steps the operations run; wired from the existing use-cases.
type (
	SourceFn   func(ctx context.Context, id domain.SourceID) error
	ActivityFn func(ctx context.Context, id domain.ActivityID) error
	UserFn     func(ctx context.Context, id domain.UserID) error
)

// pageSize bounds one keyset page of ids.
const pageSize = 200

// Runner owns the registry and the background worker.
type Runner struct {
	Repo  port.MaintenanceRepo
	Blobs port.BlobStore
	// BlobList enumerates stored objects for the garbage collection; nil
	// makes that operation unavailable.
	BlobList port.BlobLister
	// BlobPrefixes are the key families the collector may touch.
	BlobPrefixes []string

	MatchSegments      SourceFn
	ComputeBestEfforts SourceFn
	RecomputeActivity  ActivityFn
	TrainingLoad       UserFn

	Audit *audit.Recorder
	Now   func() time.Time

	mu      sync.Mutex
	jobs    map[string]*domain.MaintenanceJob
	order   []string
	cancels map[string]context.CancelFunc
	queue   chan string
	once    sync.Once
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func (r *Runner) init() {
	r.once.Do(func() {
		r.jobs = map[string]*domain.MaintenanceJob{}
		r.cancels = map[string]context.CancelFunc{}
		r.queue = make(chan string, 64)
	})
}

// Start runs queued jobs one at a time until ctx ends.
func (r *Runner) Start(ctx context.Context) {
	r.init()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case id := <-r.queue:
				r.run(ctx, id)
			}
		}
	}()
}

// Trigger validates the confirmation phrase, estimates the work and queues
// the job; Start must have been called for it to run.
func (r *Runner) Trigger(ctx context.Context, actor audit.Actor, op domain.MaintenanceOperation, users []domain.UserID, confirm string) (domain.MaintenanceJob, error) {
	r.init()
	if r.Repo == nil {
		return domain.MaintenanceJob{}, port.NewActionError(port.ActionErrorUnavailable, "maintenance is not available")
	}
	if err := op.Confirm(confirm); err != nil {
		return domain.MaintenanceJob{}, port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	if err := r.supported(op); err != nil {
		return domain.MaintenanceJob{}, err
	}
	estimate, err := r.estimate(ctx, op, users)
	if err != nil {
		return domain.MaintenanceJob{}, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("estimate: %v", err))
	}
	job := &domain.MaintenanceJob{ID: uuid.NewString(), Operation: op, UserIDs: users, Status: domain.MaintenanceQueued,
		Estimated: estimate, TriggeredBy: actor.UserID, QueuedAt: r.now()}
	r.mu.Lock()
	r.jobs[job.ID] = job
	r.order = append(r.order, job.ID)
	snapshot := *job // the worker may start mutating job as soon as it is queued
	r.mu.Unlock()
	select {
	case r.queue <- job.ID:
	default:
		r.finish(job.ID, domain.MaintenanceFailed, "maintenance queue is full")
		return snapshot, port.NewActionError(port.ActionErrorQuota, "maintenance queue is full")
	}
	r.Audit.High(ctx, actor, domain.AuditMaintenance, "maintenance_job", job.ID,
		map[string]string{"operation": string(op), "users": strings.Join(userStrings(users), ","), "estimated": fmt.Sprint(estimate)})
	return snapshot, nil
}

// supported reports why an operation cannot run on this instance.
func (r *Runner) supported(op domain.MaintenanceOperation) error {
	missing := func(what string) error {
		return port.NewActionError(port.ActionErrorUnavailable, what+" is not available on this instance")
	}
	switch op {
	case domain.MaintenanceRematchAllSegments:
		if r.MatchSegments == nil {
			return missing("segment matching")
		}
	case domain.MaintenanceRecomputeBestEfforts:
		if r.ComputeBestEfforts == nil {
			return missing("best-effort computation")
		}
	case domain.MaintenanceRecomputeActivityMerges:
		if r.RecomputeActivity == nil {
			return missing("activity recomputation")
		}
	case domain.MaintenanceRecomputeTrainingLoad:
		if r.TrainingLoad == nil {
			return missing("training load")
		}
	case domain.MaintenancePurgeSoftDeleted:
	case domain.MaintenanceGarbageCollectBlobs:
		if r.Blobs == nil || r.BlobList == nil {
			return missing("blob storage listing")
		}
	}
	return nil
}

// estimate counts what the operation will touch.
func (r *Runner) estimate(ctx context.Context, op domain.MaintenanceOperation, users []domain.UserID) (int64, error) {
	switch op {
	case domain.MaintenanceRematchAllSegments, domain.MaintenanceRecomputeBestEfforts:
		return r.Repo.CountSources(ctx, users)
	case domain.MaintenanceRecomputeActivityMerges:
		return r.Repo.CountActivities(ctx, users)
	case domain.MaintenanceRecomputeTrainingLoad:
		return r.Repo.CountUsers(ctx, users)
	case domain.MaintenancePurgeSoftDeleted:
		return r.Repo.CountSoftDeletedActivities(ctx, users, r.now().Add(-domain.SoftDeleteRetention))
	default:
		return -1, nil // blob GC: unknown until listed
	}
}

// List returns every job, newest first.
func (r *Runner) List() []domain.MaintenanceJob {
	r.init()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.MaintenanceJob, 0, len(r.order))
	for i := len(r.order) - 1; i >= 0; i-- {
		out = append(out, *r.jobs[r.order[i]])
	}
	return out
}

// Get returns one job.
func (r *Runner) Get(id string) (domain.MaintenanceJob, error) {
	r.init()
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[id]
	if !ok {
		return domain.MaintenanceJob{}, port.NewActionError(port.ActionErrorNotFound, "maintenance job not found")
	}
	return *job, nil
}

// Cancel stops a queued or running job.
func (r *Runner) Cancel(ctx context.Context, actor audit.Actor, id string) (domain.MaintenanceJob, error) {
	r.init()
	r.mu.Lock()
	job, ok := r.jobs[id]
	if !ok {
		r.mu.Unlock()
		return domain.MaintenanceJob{}, port.NewActionError(port.ActionErrorNotFound, "maintenance job not found")
	}
	if job.Status.Done() {
		r.mu.Unlock()
		return *job, port.NewActionError(port.ActionErrorPrecondition, "job already finished")
	}
	cancel := r.cancels[id]
	if job.Status == domain.MaintenanceQueued {
		now := r.now()
		job.Status, job.FinishedAt = domain.MaintenanceCancelled, &now
	}
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.Audit.Info(ctx, actor, domain.AuditJobCancelled, "maintenance_job", id, nil)
	return r.Get(id)
}

// run executes one job to completion.
func (r *Runner) run(base context.Context, id string) {
	r.mu.Lock()
	job, ok := r.jobs[id]
	if !ok || job.Status != domain.MaintenanceQueued {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(base)
	now := r.now()
	job.Status, job.StartedAt = domain.MaintenanceRunning, &now
	r.cancels[id] = cancel
	op, users := job.Operation, job.UserIDs
	r.mu.Unlock()
	defer cancel()

	err := r.execute(ctx, id, op, users)
	switch {
	case err == nil:
		r.finish(id, domain.MaintenanceSucceeded, "")
	case errors.Is(err, context.Canceled) && base.Err() == nil:
		r.finish(id, domain.MaintenanceCancelled, "")
	default:
		r.finish(id, domain.MaintenanceFailed, err.Error())
	}
}

func (r *Runner) finish(id string, status domain.MaintenanceStatus, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job := r.jobs[id]
	now := r.now()
	job.Status, job.FinishedAt, job.Error = status, &now, msg
	delete(r.cancels, id)
}

// progress bumps the counters of a running job.
func (r *Runner) progress(id string, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job := r.jobs[id]
	job.Processed++
	if failed {
		job.Failed++
	}
}

func (r *Runner) setEstimate(id string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[id].Estimated = n
}

func (r *Runner) execute(ctx context.Context, id string, op domain.MaintenanceOperation, users []domain.UserID) error {
	switch op {
	case domain.MaintenanceRematchAllSegments:
		return r.eachSource(ctx, id, users, r.MatchSegments)
	case domain.MaintenanceRecomputeBestEfforts:
		return r.eachSource(ctx, id, users, r.ComputeBestEfforts)
	case domain.MaintenanceRecomputeActivityMerges:
		return r.eachActivity(ctx, id, users, r.RecomputeActivity)
	case domain.MaintenanceRecomputeTrainingLoad:
		return r.eachUser(ctx, id, users)
	case domain.MaintenancePurgeSoftDeleted:
		return r.purgeSoftDeleted(ctx, id, users)
	case domain.MaintenanceGarbageCollectBlobs:
		return r.collectBlobs(ctx, id)
	}
	return domain.ErrUnknownMaintenanceOperation
}

func userStrings(ids []domain.UserID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}
