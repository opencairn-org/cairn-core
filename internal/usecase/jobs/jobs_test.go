package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

type fakeQueue struct {
	port.ImportQueueAdmin
	rows map[domain.ImportQueueItemID]*domain.ImportQueueItem
}

func (f *fakeQueue) ListAll(_ context.Context, fl port.ImportQueueFilter) ([]domain.ImportQueueItem, int, error) {
	var out []domain.ImportQueueItem
	for _, it := range f.rows {
		if len(fl.Statuses) > 0 && it.Status != fl.Statuses[0] {
			continue
		}
		out = append(out, *it)
	}
	return out, len(out), nil
}
func (f *fakeQueue) CountAllByStatus(context.Context, port.ImportQueueFilter) (map[domain.ImportItemStatus]int, error) {
	out := map[domain.ImportItemStatus]int{}
	for _, it := range f.rows {
		out[it.Status]++
	}
	return out, nil
}
func (f *fakeQueue) Get(_ context.Context, id domain.ImportQueueItemID) (domain.ImportQueueItem, error) {
	if it, ok := f.rows[id]; ok {
		return *it, nil
	}
	return domain.ImportQueueItem{}, domain.ErrNotFound
}
func (f *fakeQueue) Retry(_ context.Context, id domain.ImportQueueItemID, reset bool) (bool, error) {
	it := f.rows[id]
	if it.Status != domain.ImportStatusFailed && it.Status != domain.ImportStatusSkipped {
		return false, nil
	}
	it.Status = domain.ImportStatusPending
	if reset {
		it.Attempts = 0
	}
	return true, nil
}
func (f *fakeQueue) Cancel(_ context.Context, id domain.ImportQueueItemID, reason string) (bool, error) {
	it := f.rows[id]
	if it.Status != domain.ImportStatusPending && it.Status != domain.ImportStatusInProgress {
		return false, nil
	}
	it.Status, it.LastError = domain.ImportStatusSkipped, reason
	return true, nil
}

type fakeDLQ struct {
	port.DeadLetterRepo
	rows     map[domain.DeadLetteredJobID]*domain.DeadLetteredJob
	replayed []domain.DeadLetteredJobID
}

func (f *fakeDLQ) List(context.Context, port.DeadLetterFilter) ([]domain.DeadLetteredJob, error) {
	var out []domain.DeadLetteredJob
	for _, j := range f.rows {
		out = append(out, *j)
	}
	return out, nil
}
func (f *fakeDLQ) Get(_ context.Context, id domain.DeadLetteredJobID) (domain.DeadLetteredJob, error) {
	if j, ok := f.rows[id]; ok {
		return *j, nil
	}
	return domain.DeadLetteredJob{}, domain.ErrNotFound
}
func (f *fakeDLQ) MarkReplayed(_ context.Context, id domain.DeadLetteredJobID, _ *domain.UserID, at time.Time) error {
	f.rows[id].ReplayedAt = &at
	f.replayed = append(f.replayed, id)
	return nil
}

type fakeReplay struct{ msgs []string }

func (f *fakeReplay) Replay(_ context.Context, job domain.DeadLetteredJob) (string, error) {
	f.msgs = append(f.msgs, job.MsgID)
	return "replay:" + job.MsgID, nil
}

type auditSpy struct {
	port.AuditRepo
	actions []string
}

func (a *auditSpy) Record(_ context.Context, e domain.AuditEntry) error {
	a.actions = append(a.actions, e.Action)
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

func TestJobsQueue(t *testing.T) {
	failed, running := domain.ImportQueueItemID(uuid.New()), domain.ImportQueueItemID(uuid.New())
	q := &fakeQueue{rows: map[domain.ImportQueueItemID]*domain.ImportQueueItem{
		failed:  {ID: failed, Status: domain.ImportStatusFailed, Attempts: 3},
		running: {ID: running, Status: domain.ImportStatusInProgress},
	}}
	spy := &auditSpy{}
	j := &Jobs{Queue: q, Audit: &audit.Recorder{Repo: spy}}
	ctx := t.Context()

	items, total, counts, err := j.List(ctx, port.ImportQueueFilter{})
	if err != nil || total != 2 || len(items) != 2 || counts[domain.ImportStatusFailed] != 1 {
		t.Fatalf("list: %v %d %v", err, total, counts)
	}
	if _, err := j.Retry(ctx, audit.Actor{}, running, false); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("retry running: %v", err)
	}
	got, err := j.Retry(ctx, audit.Actor{}, failed, true)
	if err != nil || got.Status != domain.ImportStatusPending || got.Attempts != 0 {
		t.Fatalf("retry: %v %+v", err, got)
	}
	if _, err := j.Cancel(ctx, audit.Actor{}, failed); err != nil || q.rows[failed].Status != domain.ImportStatusSkipped {
		t.Fatalf("cancel pending: %v", err)
	}
	if _, err := j.Cancel(ctx, audit.Actor{}, failed); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("cancel twice: %v", err)
	}
	if _, err := j.Get(ctx, domain.ImportQueueItemID(uuid.New())); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("unknown: %v", err)
	}
	if len(spy.actions) != 2 || spy.actions[0] != domain.AuditJobRetried || spy.actions[1] != domain.AuditJobCancelled {
		t.Fatalf("audit %v", spy.actions)
	}
	if _, _, _, err := (&Jobs{}).List(ctx, port.ImportQueueFilter{}); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("unwired: %v", err)
	}
}

func TestJobsDeadLetters(t *testing.T) {
	withPayload, empty := domain.DeadLetteredJobID(uuid.New()), domain.DeadLetteredJobID(uuid.New())
	dlq := &fakeDLQ{rows: map[domain.DeadLetteredJobID]*domain.DeadLetteredJob{
		withPayload: {ID: withPayload, MsgID: "m1", Payload: []byte(`{}`)},
		empty:       {ID: empty, MsgID: "m2"},
	}}
	replay := &fakeReplay{}
	j := &Jobs{DeadLetters: dlq, Replay: replay, Audit: &audit.Recorder{Repo: &auditSpy{}}}
	ctx := t.Context()

	list, err := j.ListDeadLetters(ctx, port.DeadLetterFilter{})
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	if _, _, err := j.Requeue(ctx, audit.Actor{}, empty); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("no payload: %v", err)
	}
	job, msg, err := j.Requeue(ctx, audit.Actor{}, withPayload)
	if err != nil || msg != "replay:m1" || job.ReplayedAt == nil || len(replay.msgs) != 1 {
		t.Fatalf("requeue: %v %q %+v", err, msg, job)
	}
	if _, _, err := j.Requeue(ctx, audit.Actor{}, domain.DeadLetteredJobID(uuid.New())); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("unknown: %v", err)
	}
}

type fakeSources struct {
	port.ActivityAdminStats
	groups []port.SourceWorkerGroup
}

func (f *fakeSources) SourceWorkerGroups(context.Context) ([]port.SourceWorkerGroup, error) {
	return f.groups, nil
}

type fakePools struct {
	port.WorkerPoolRepo
	pools []domain.WorkerPool
}

func (f *fakePools) ListAll(context.Context) ([]domain.WorkerPool, error) { return f.pools, nil }

func TestOrphans(t *testing.T) {
	retired := time.Now()
	o := &Orphans{
		Activities:  &fakeSources{groups: []port.SourceWorkerGroup{{WorkerName: "live", Sources: 5}, {WorkerName: "gone", Sources: 2}, {WorkerName: "retired", Sources: 1}, {WorkerName: "registered", Sources: 3}, {WorkerName: "", Sources: 9}}},
		Pools:       &fakePools{pools: []domain.WorkerPool{{Name: "registered"}, {Name: "retired", RetiredAt: &retired}}},
		LiveWorkers: func(context.Context) []string { return []string{"live"} },
	}
	groups, err := o.List(t.Context(), "")
	if err != nil || len(groups) != 2 {
		t.Fatalf("orphans: %v %+v", err, groups)
	}
	names := map[string]bool{}
	for _, g := range groups {
		names[g.WorkerName] = true
	}
	if !names["gone"] || !names["retired"] {
		t.Fatalf("orphans %v", names)
	}
	if only, _ := o.List(t.Context(), "gone"); len(only) != 1 || only[0].Sources != 2 {
		t.Fatalf("filter %+v", only)
	}
}
