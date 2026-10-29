package connect

import (
	"context"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

type captureAdminEvents struct{ got chan *v1.AdminEvent }

func (c *captureAdminEvents) Send(ev *v1.AdminEvent) error { c.got <- ev; return nil }

func TestStreamAdminEvents(t *testing.T) {
	hub := NewAdminEventHub()
	s := &AdminServer{Events: hub}
	admin := domain.UserID(uuid.New())
	if err := s.streamAdminEvents(t.Context(), &v1.StreamAdminEventsRequest{}, &captureAdminEvents{}); connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
	if err := (&AdminServer{}).streamAdminEvents(context.WithValue(t.Context(), ctxKeyUserID, admin), &v1.StreamAdminEventsRequest{}, &captureAdminEvents{}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("no hub: %v", err)
	}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxKeyUserID, admin))
	defer cancel()
	sink := &captureAdminEvents{got: make(chan *v1.AdminEvent, 8)}
	done := make(chan error, 1)
	go func() {
		done <- s.streamAdminEvents(ctx, &v1.StreamAdminEventsRequest{Types: []v1.AdminEventType{v1.AdminEventType_ADMIN_EVENT_TYPE_WORKER_OFFLINE}}, sink)
	}()
	for hub.Subscribers(struct{}{}) == 0 {
		time.Sleep(time.Millisecond)
	}
	hub.Publish(domain.NewAdminEvent(domain.AdminEventUserCreated, domain.AuditInfo, "admin.user_created").AboutUser(admin))
	hub.Publish(domain.NewAdminEvent(domain.AdminEventWorkerOffline, domain.AuditWarn, "admin.worker_offline").AboutWorker("strava-1").AboutJob("j1"))
	got := <-sink.got
	if got.GetType() != v1.AdminEventType_ADMIN_EVENT_TYPE_WORKER_OFFLINE || got.GetWorkerName() != "strava-1" || got.GetJobId() != "j1" ||
		got.GetSeverity() != v1.AuditSeverity_AUDIT_SEVERITY_WARN || got.GetI18NParams()["worker"] != "strava-1" || got.GetId() == "" {
		t.Fatalf("event: %+v", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("stream end: %v", err)
	}
}
