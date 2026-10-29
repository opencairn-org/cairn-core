package connect

import (
	"context"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/livefeed"
)

type captureActivityEvents struct {
	got  chan *v1.ActivityEvent
	fail bool
}

func (c *captureActivityEvents) Send(ev *v1.ActivityEvent) error {
	if c.fail {
		return context.Canceled
	}
	c.got <- ev
	return nil
}

func newActivityHub() *ActivityEventHub {
	return livefeed.New(func(e domain.ActivityEvent) domain.UserID { return e.UserID })
}

func TestStreamActivityEvents(t *testing.T) {
	hub := newActivityHub()
	s := &ActivityServer{Events: hub}
	me, other := domain.UserID(uuid.New()), domain.UserID(uuid.New())

	if err := s.streamActivityEvents(t.Context(), &v1.StreamActivityEventsRequest{}, &captureActivityEvents{}); connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
	if err := (&ActivityServer{}).streamActivityEvents(context.WithValue(t.Context(), ctxKeyUserID, me), &v1.StreamActivityEventsRequest{}, &captureActivityEvents{}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("no hub: %v", err)
	}

	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxKeyUserID, me))
	defer cancel()
	sink := &captureActivityEvents{got: make(chan *v1.ActivityEvent, 8)}
	done := make(chan error, 1)
	go func() {
		done <- s.streamActivityEvents(ctx, &v1.StreamActivityEventsRequest{
			Types: []v1.ActivityEventType{v1.ActivityEventType_ACTIVITY_EVENT_TYPE_ACTIVITY_DELETED},
		}, sink)
	}()
	for hub.Subscribers(me) == 0 {
		time.Sleep(time.Millisecond)
	}
	actID := domain.ActivityID(uuid.New())
	hub.Publish(domain.NewActivityEvent(domain.ActivityEventActivityDeleted, other, "x").For(actID, domain.SourceID{}))
	hub.Publish(domain.NewActivityEvent(domain.ActivityEventImportCompleted, me, "activity.import_completed").For(actID, domain.SourceID{}))
	hub.Publish(domain.NewActivityEvent(domain.ActivityEventActivityDeleted, me, "activity.deleted").For(actID, domain.SourceID{}).With("k", "v"))

	select {
	case ev := <-sink.got:
		if ev.GetType() != v1.ActivityEventType_ACTIVITY_EVENT_TYPE_ACTIVITY_DELETED || ev.GetActivityId() != actID.String() || ev.GetI18NParams()["k"] != "v" {
			t.Fatalf("event %v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event forwarded")
	}
	select {
	case ev := <-sink.got:
		t.Fatalf("filtered event leaked: %v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("stream end: %v", err)
	}
}
