package maintenance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

type fakeRepo struct {
	port.MaintenanceRepo
	sources    []domain.SourceID
	activities []domain.ActivityID
	users      []domain.UserID
	deleted    []domain.ActivityID
	referenced map[string]bool
	mu         sync.Mutex
	purged     []domain.ActivityID
}

func page[T interface{ String() string }](all []T, after T, limit int) []T {
	var out []T
	started := after.String() == uuid.Nil.String()
	for _, id := range all {
		if !started {
			started = id.String() == after.String()
			continue
		}
		out = append(out, id)
		if len(out) == limit {
			break
		}
	}
	return out
}

func (f *fakeRepo) CountSources(context.Context, []domain.UserID) (int64, error) {
	return int64(len(f.sources)), nil
}
func (f *fakeRepo) ListSourceIDs(_ context.Context, _ []domain.UserID, after domain.SourceID, limit int) ([]domain.SourceID, error) {
	return page(f.sources, after, limit), nil
}
func (f *fakeRepo) CountActivities(context.Context, []domain.UserID) (int64, error) {
	return int64(len(f.activities)), nil
}
func (f *fakeRepo) ListActivityIDs(_ context.Context, _ []domain.UserID, after domain.ActivityID, limit int) ([]domain.ActivityID, error) {
	return page(f.activities, after, limit), nil
}
func (f *fakeRepo) CountUsers(context.Context, []domain.UserID) (int64, error) {
	return int64(len(f.users)), nil
}
func (f *fakeRepo) ListUserIDs(_ context.Context, _ []domain.UserID, after domain.UserID, limit int) ([]domain.UserID, error) {
	return page(f.users, after, limit), nil
}
func (f *fakeRepo) CountSoftDeletedActivities(context.Context, []domain.UserID, time.Time) (int64, error) {
	return int64(len(f.deleted)), nil
}
func (f *fakeRepo) ListSoftDeletedActivityIDs(_ context.Context, _ []domain.UserID, _ time.Time, after domain.ActivityID, limit int) ([]domain.ActivityID, error) {
	return page(f.deleted, after, limit), nil
}
func (f *fakeRepo) HardDeleteActivity(_ context.Context, id domain.ActivityID) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purged = append(f.purged, id)
	return []string{"raw/" + id.String()}, nil
}
func (f *fakeRepo) ReferencedBlobKeys(_ context.Context, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, k := range keys {
		out[k] = f.referenced[k]
	}
	return out, nil
}

type fakeBlobs struct {
	port.BlobStore
	objects map[string]time.Time
	mu      sync.Mutex
	deleted []string
}

func (b *fakeBlobs) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleted = append(b.deleted, key)
	return nil
}
func (b *fakeBlobs) ListBlobKeys(_ context.Context, prefix string, visit func(string, time.Time) error) error {
	for k, at := range b.objects {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			if err := visit(k, at); err != nil {
				return err
			}
		}
	}
	return nil
}

func kindOf(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

func waitDone(t *testing.T, r *Runner, id string) domain.MaintenanceJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err := r.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status.Done() {
			return job
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("job never finished")
	return domain.MaintenanceJob{}
}

func ids[T any](n int, mk func(uuid.UUID) T) []T {
	out := make([]T, 0, n)
	for range n {
		out = append(out, mk(uuid.New()))
	}
	return out
}

func TestRunnerTriggerAndRun(t *testing.T) {
	ctx := t.Context()
	repo := &fakeRepo{sources: ids(pageSize+3, func(u uuid.UUID) domain.SourceID { return domain.SourceID(u) }),
		users: ids(2, func(u uuid.UUID) domain.UserID { return domain.UserID(u) })}
	var matched, failedOnce int
	var mu sync.Mutex
	r := &Runner{Repo: repo,
		MatchSegments: func(context.Context, domain.SourceID) error {
			mu.Lock()
			defer mu.Unlock()
			matched++
			if matched == 1 {
				failedOnce++
				return errors.New("boom")
			}
			return nil
		},
		TrainingLoad: func(context.Context, domain.UserID) error { return nil }}
	r.Start(ctx)

	if _, err := r.Trigger(ctx, audit.Actor{}, domain.MaintenanceRematchAllSegments, nil, "nope"); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("bad phrase: %v", err)
	}
	if _, err := r.Trigger(ctx, audit.Actor{}, domain.MaintenanceOperation("x"), nil, "X"); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("unknown op: %v", err)
	}
	if _, err := r.Trigger(ctx, audit.Actor{}, domain.MaintenanceRecomputeBestEfforts, nil, "RECOMPUTE_BEST_EFFORTS"); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("unwired op: %v", err)
	}
	job, err := r.Trigger(ctx, audit.Actor{}, domain.MaintenanceRematchAllSegments, nil, " REMATCH_ALL_SEGMENTS ")
	if err != nil || job.Estimated != int64(pageSize+3) || job.Status != domain.MaintenanceQueued {
		t.Fatalf("trigger: %v %+v", err, job)
	}
	done := waitDone(t, r, job.ID)
	if done.Status != domain.MaintenanceSucceeded || done.Processed != int64(pageSize+3) || done.Failed != 1 || done.StartedAt == nil {
		t.Fatalf("rematch: %+v", done)
	}
	tl, _ := r.Trigger(ctx, audit.Actor{}, domain.MaintenanceRecomputeTrainingLoad, nil, "RECOMPUTE_TRAINING_LOAD")
	if done := waitDone(t, r, tl.ID); done.Processed != 2 {
		t.Fatalf("training load: %+v", done)
	}
	if list := r.List(); len(list) != 2 || list[0].ID != tl.ID {
		t.Fatalf("list newest first: %+v", list)
	}
	if _, err := r.Cancel(ctx, audit.Actor{}, tl.ID); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("cancel finished: %v", err)
	}
	if _, err := r.Get("missing"); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("get missing: %v", err)
	}
}

func TestRunnerCancelRunning(t *testing.T) {
	ctx := t.Context()
	repo := &fakeRepo{activities: ids(50, func(u uuid.UUID) domain.ActivityID { return domain.ActivityID(u) })}
	started := make(chan struct{})
	var once sync.Once
	r := &Runner{Repo: repo, RecomputeActivity: func(ctx context.Context, _ domain.ActivityID) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	}}
	r.Start(ctx)
	job, err := r.Trigger(ctx, audit.Actor{}, domain.MaintenanceRecomputeActivityMerges, nil, "RECOMPUTE_ACTIVITY_MERGES")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := r.Cancel(ctx, audit.Actor{}, job.ID); err != nil {
		t.Fatal(err)
	}
	if done := waitDone(t, r, job.ID); done.Status != domain.MaintenanceCancelled || done.Processed > 1 {
		t.Fatalf("cancelled: %+v", done)
	}
}

func TestRunnerPurgeAndGC(t *testing.T) {
	ctx := t.Context()
	now := time.Now().UTC()
	gone := domain.ActivityID(uuid.New())
	stale, fresh, live, other := "raw/old", "raw/new", "attachments/live", "transfer/x"
	blobs := &fakeBlobs{objects: map[string]time.Time{stale: now.Add(-48 * time.Hour), fresh: now, live: now.Add(-48 * time.Hour), other: now.Add(-48 * time.Hour)}}
	repo := &fakeRepo{deleted: []domain.ActivityID{gone}, referenced: map[string]bool{live: true}}
	r := &Runner{Repo: repo, Blobs: blobs, BlobList: blobs, BlobPrefixes: []string{"raw/", "attachments/"}, Now: func() time.Time { return now }}
	r.Start(ctx)

	job, err := r.Trigger(ctx, audit.Actor{}, domain.MaintenancePurgeSoftDeleted, nil, "PURGE_SOFT_DELETED")
	if err != nil || job.Estimated != 1 {
		t.Fatalf("purge trigger: %v %+v", err, job)
	}
	if done := waitDone(t, r, job.ID); done.Processed != 1 || len(repo.purged) != 1 || len(blobs.deleted) != 1 || blobs.deleted[0] != "raw/"+gone.String() {
		t.Fatalf("purge: %+v %v", done, blobs.deleted)
	}
	blobs.deleted = nil
	gc, err := r.Trigger(ctx, audit.Actor{}, domain.MaintenanceGarbageCollectBlobs, nil, "GARBAGE_COLLECT_BLOBS")
	if err != nil || gc.Estimated != -1 {
		t.Fatalf("gc trigger: %v %+v", err, gc)
	}
	if done := waitDone(t, r, gc.ID); done.Processed != 1 || done.Estimated != 3 || len(blobs.deleted) != 1 || blobs.deleted[0] != stale {
		t.Fatalf("gc: %+v %v", done, blobs.deleted)
	}
	if _, err := (&Runner{Repo: repo}).Trigger(ctx, audit.Actor{}, domain.MaintenanceGarbageCollectBlobs, nil, "GARBAGE_COLLECT_BLOBS"); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("gc without lister: %v", err)
	}
}
