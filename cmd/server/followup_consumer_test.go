package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	"github.com/opencairn-org/cairn-core/internal/usecase/pipeline"
)

type fedDeletes struct {
	mu      sync.Mutex
	deleted []domain.ActivityID
}

func (f *fedDeletes) PublishCreate(context.Context, domain.Activity) {}
func (f *fedDeletes) PublishDelete(_ context.Context, _ domain.UserID, id domain.ActivityID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
}

func (f *fedDeletes) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deleted)
}

// followUpPipeline holds one activity whose only source is detached, so a remerge soft-deletes it.
func followUpPipeline(t *testing.T) (*pipeline.Pipeline, *fedDeletes, domain.ActivityID) {
	t.Helper()
	repo := inmem.NewActivityRepo()
	now := time.Now().UTC()
	uid, aid, sid := domain.UserID(uuid.New()), domain.ActivityID(uuid.New()), domain.SourceID(uuid.New())
	repo.Activities[aid] = domain.Activity{ID: aid, UserID: uid, Type: domain.ActivityTypeRide, StartTime: now, Privacy: domain.PrivacyPrivate}
	repo.Sources[sid] = domain.ActivitySource{ID: sid, ActivityID: aid, UserID: uid, Provider: "garmin", ExternalID: "x", Status: domain.SourceStatusDetached, ImportedAt: now}
	fed := &fedDeletes{}
	recompute := activity.NewRecomputeActivityFromSources(repo, inmem.DefaultSettings{}, nil, nil, nil, nil, inmem.PassTx{}, time.Now)
	return &pipeline.Pipeline{Activities: repo, Recompute: recompute, Federation: fed}, fed, aid
}

func TestFollowUpEventRoundTrip(t *testing.T) {
	bus, _ := dlqTestBus(t)
	ctx := t.Context()
	if err := bus.BootstrapStreams(ctx); err != nil {
		t.Fatal(err)
	}
	p, fed, aid := followUpPipeline(t)
	p.Bus = bus
	if _, err := p.Remerge(ctx, aid); err != nil {
		t.Fatal(err)
	}
	// Published, not run inline; the consumer created afterwards still picks it up.
	if fed.count() != 0 {
		t.Fatal("follow-ups ran inline")
	}
	app := &App{NATSBus: bus, Pipeline: p}
	sub, err := startFollowUpConsumer(ctx, app, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close(context.Background()) })
	deadline := time.Now().Add(10 * time.Second)
	for fed.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if fed.count() != 1 {
		t.Fatalf("follow-up did not run via the bus: %d deletes", fed.count())
	}
}

func TestFollowUpHandler(t *testing.T) {
	p, fed, aid := followUpPipeline(t)
	h := followUpHandler(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var term *port.TerminalError
	if err := h(t.Context(), port.Message{Body: []byte(`{"kind":"nope"}`)}); !errors.As(err, &term) {
		t.Fatalf("bad payload: %v", err)
	}
	body, err := pipeline.ActivityChanged{Kind: pipeline.ChangeRemerged, UserID: domain.UserID(uuid.New()), ActivityID: aid, SoftDeleted: true}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// A redelivery must not re-announce to remote followers.
	if err := h(t.Context(), port.Message{Body: body, DeliveryAttempt: 2}); err != nil || fed.count() != 0 {
		t.Fatalf("redelivery: %v deletes=%d", err, fed.count())
	}
	if err := h(t.Context(), port.Message{Body: body, DeliveryAttempt: 1}); err != nil || fed.count() != 1 {
		t.Fatalf("first delivery: %v deletes=%d", err, fed.count())
	}
}
