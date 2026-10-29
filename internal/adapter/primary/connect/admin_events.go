package connect

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/livefeed"
)

// AdminEventHub is the single-channel live feed behind StreamAdminEvents.
type AdminEventHub = livefeed.Hub[struct{}, domain.AdminEvent]

// NewAdminEventHub returns a hub where every event lands on the one channel.
func NewAdminEventHub() *AdminEventHub {
	return livefeed.New(func(domain.AdminEvent) struct{} { return struct{}{} })
}

// StreamAdminEvents forwards operator events until the client goes away;
// an empty heartbeat every 30 s keeps idle proxies from closing the stream.
func (s *AdminServer) StreamAdminEvents(ctx context.Context, req *v1.StreamAdminEventsRequest, stream cairnv1connect.AdminServiceStreamAdminEventsServerStream) error {
	return s.streamAdminEvents(ctx, req, stream)
}

// adminEventSender is the send side of the stream (generated, or a test double).
type adminEventSender interface {
	Send(*v1.AdminEvent) error
}

func (s *AdminServer) streamAdminEvents(ctx context.Context, req *v1.StreamAdminEventsRequest, stream adminEventSender) error {
	if _, err := userIDFromHandlerCtx(ctx); err != nil {
		return err
	}
	if s.Events == nil {
		return unavailable("the admin event stream")
	}
	events, cancel := s.Events.Subscribe(struct{}{})
	defer cancel()
	keep := adminEventFilter(req)
	return pumpStream(ctx, events,
		func() error { return stream.Send(&v1.AdminEvent{Ts: timestamppb.Now()}) },
		func(ev domain.AdminEvent) error {
			if !keep(ev) {
				return nil
			}
			return stream.Send(adminEventToProto(ev))
		})
}

// adminEventFilter keeps the requested types (all when none are given).
func adminEventFilter(req *v1.StreamAdminEventsRequest) func(domain.AdminEvent) bool {
	types := map[v1.AdminEventType]bool{}
	for _, t := range req.GetTypes() {
		types[t] = true
	}
	return func(ev domain.AdminEvent) bool {
		return len(types) == 0 || types[adminEventTypes[ev.Type]]
	}
}

func adminEventToProto(ev domain.AdminEvent) *v1.AdminEvent {
	out := &v1.AdminEvent{
		Id: ev.ID, Type: adminEventTypes[ev.Type], Severity: auditSeverityToProto(ev.Severity),
		Ts: timestamppb.New(ev.At), I18NKey: ev.Key, I18NParams: ev.Params,
	}
	if ev.UserID != nil {
		s := ev.UserID.String()
		out.UserId = &s
	}
	if ev.WorkerName != "" {
		out.WorkerName = &ev.WorkerName
	}
	if ev.JobID != "" {
		out.JobId = &ev.JobID
	}
	return out
}

var adminEventTypes = map[domain.AdminEventType]v1.AdminEventType{
	domain.AdminEventWorkerRegistered:     v1.AdminEventType_ADMIN_EVENT_TYPE_WORKER_REGISTERED,
	domain.AdminEventWorkerDeregistered:   v1.AdminEventType_ADMIN_EVENT_TYPE_WORKER_DEREGISTERED,
	domain.AdminEventWorkerOffline:        v1.AdminEventType_ADMIN_EVENT_TYPE_WORKER_OFFLINE,
	domain.AdminEventWorkerDegraded:       v1.AdminEventType_ADMIN_EVENT_TYPE_WORKER_DEGRADED,
	domain.AdminEventProviderConflict:     v1.AdminEventType_ADMIN_EVENT_TYPE_PROVIDER_CONFLICT_DETECTED,
	domain.AdminEventJobDeadLettered:      v1.AdminEventType_ADMIN_EVENT_TYPE_JOB_DEAD_LETTERED,
	domain.AdminEventUserCreated:          v1.AdminEventType_ADMIN_EVENT_TYPE_USER_CREATED,
	domain.AdminEventUserSuspended:        v1.AdminEventType_ADMIN_EVENT_TYPE_USER_SUSPENDED,
	domain.AdminEventAuthFloodDetected:    v1.AdminEventType_ADMIN_EVENT_TYPE_AUTH_FLOOD_DETECTED,
	domain.AdminEventSystemHealthDegraded: v1.AdminEventType_ADMIN_EVENT_TYPE_SYSTEM_HEALTH_DEGRADED,
}
