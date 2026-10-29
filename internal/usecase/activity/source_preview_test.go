package activity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeReimporter struct{ jobs []string }

func (f *fakeReimporter) Refetch(_ context.Context, _ domain.UserID, src domain.ActivitySource) (string, error) {
	f.jobs = append(f.jobs, "refetch:"+src.ID.String())
	return f.jobs[len(f.jobs)-1], nil
}
func (f *fakeReimporter) Reparse(_ context.Context, _ domain.UserID, src domain.ActivitySource) (string, error) {
	f.jobs = append(f.jobs, "reparse:"+src.ID.String())
	return f.jobs[len(f.jobs)-1], nil
}

type eventSpy struct{ keys []string }

func (e *eventSpy) Publish(ev domain.ActivityEvent) { e.keys = append(e.keys, ev.Key) }

func previewFixture(t *testing.T) (*SourcePreviews, *saFixture, *inmem.SourcePreviewRepo, *fakeReimporter, *eventSpy) {
	t.Helper()
	f := newSAFixture(t, 1)
	repo := inmem.NewSourcePreviewRepo()
	repo.ActivityOf = func(domain.SourceID) domain.ActivityID { return f.act }
	ri := &fakeReimporter{}
	ev := &eventSpy{}
	recompute := NewRecomputeActivityFromSources(f.repo, inmem.DefaultSettings{}, nil, nil, nil, nil, inmem.PassTx{}, func() time.Time { return saNow })
	p := &SourcePreviews{Repo: repo, Actions: f.uc, Recompute: recompute, Reimporter: ri, Activities: f.repo, Events: ev,
		Now: func() time.Time { return saNow }}
	return p, f, repo, ri, ev
}

func kindOf(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

func TestSourcePreviewLifecycle(t *testing.T) {
	p, f, repo, ri, ev := previewFixture(t)
	ctx := t.Context()
	sid := f.srcs[0]

	prev, err := p.Start(ctx, f.user, f.act, sid, true)
	if err != nil {
		t.Fatal(err)
	}
	if prev.Status != domain.SourcePreviewPending || prev.JobID != "reparse:"+sid.String() || len(ri.jobs) != 1 {
		t.Fatalf("start %+v jobs %v", prev, ri.jobs)
	}
	if _, err := p.Start(ctx, f.user, f.act, sid, false); kindOf(t, err) != port.ActionErrorConflict {
		t.Fatalf("second start: %v", err)
	}
	// Still pending: not committable.
	if _, err := p.TakeForCommit(ctx, f.user, prev.ID); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("pending commit: %v", err)
	}

	// The worker answers with a longer ride.
	src := f.repo.Sources[sid]
	proposed := src.Parsed
	d := 12000.0
	proposed.Summary.DistanceM = &d
	proposed.Title = "Longer"
	claimed, err := p.Capture(ctx, src, proposed, []byte(`{"held":true}`))
	if err != nil || !claimed {
		t.Fatalf("capture: %v %v", claimed, err)
	}
	got, err := p.Get(ctx, f.user, prev.ID)
	if err != nil || got.Status != domain.SourcePreviewReady || string(got.Parsed) != `{"held":true}` {
		t.Fatalf("ready %+v %v", got, err)
	}
	if !hasChange(got.SourceDiff, "distance_m") || !hasChange(got.SourceDiff, "title") {
		t.Fatalf("source diff %+v", got.SourceDiff)
	}
	if !hasChange(got.MergedDiff, "distance_m") || hasChange(got.MergedDiff, "title") {
		t.Fatalf("merged diff keeps the user's title: %+v", got.MergedDiff)
	}
	if len(ev.keys) != 1 || ev.keys[0] != "activity.preview_ready" {
		t.Fatalf("events %v", ev.keys)
	}
	// A second result for the source is not claimed (no pending preview).
	if claimed, _ := p.Capture(ctx, src, proposed, nil); claimed {
		t.Fatal("ready preview must not claim again")
	}
	// Foreign user sees nothing.
	if _, err := p.Get(ctx, domain.UserID{}, prev.ID); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("foreign: %v", err)
	}
	taken, err := p.TakeForCommit(ctx, f.user, prev.ID)
	if err != nil || string(taken.Parsed) != `{"held":true}` || len(repo.Rows) != 0 {
		t.Fatalf("take %v %v rows %d", taken, err, len(repo.Rows))
	}
}

func TestSourcePreviewDiscardAndExpiry(t *testing.T) {
	p, f, repo, _, _ := previewFixture(t)
	ctx := t.Context()
	prev, err := p.Start(ctx, f.user, f.act, f.srcs[0], false)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Discard(ctx, f.user, prev.ID); err != nil || len(repo.Rows) != 0 {
		t.Fatalf("discard %v", err)
	}
	prev, _ = p.Start(ctx, f.user, f.act, f.srcs[0], false)
	p.Now = func() time.Time { return saNow.Add(2 * time.Hour) }
	if _, err := p.Get(ctx, f.user, prev.ID); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("expired: %v", err)
	}
}

func hasChange(c []domain.FieldChange, field string) bool {
	for _, x := range c {
		if x.Field == field {
			return true
		}
	}
	return false
}
