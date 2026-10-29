package connect

import (
	"context"

	connectrpc "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/livefeed"
)

// ActivityEventHub is the per-user live feed the stream RPC subscribes to.
type ActivityEventHub = livefeed.Hub[domain.UserID, domain.ActivityEvent]

// StreamActivityEvents forwards the caller's live activity events until the
// client goes away; every 30 s an empty heartbeat keeps proxies from
// closing the idle stream.
func (s *ActivityServer) StreamActivityEvents(ctx context.Context, req *v1.StreamActivityEventsRequest, stream cairnv1connect.ActivityServiceStreamActivityEventsServerStream) error {
	return s.streamActivityEvents(ctx, req, stream)
}

// activityEventSender is the send side of the stream (generated, or a test double).
type activityEventSender interface {
	Send(*v1.ActivityEvent) error
}

func (s *ActivityServer) streamActivityEvents(ctx context.Context, req *v1.StreamActivityEventsRequest, stream activityEventSender) error {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return err
	}
	if s.Events == nil {
		return connectrpc.NewError(connectrpc.CodeUnavailable, "live activity events are not available on this instance")
	}
	events, cancel := s.Events.Subscribe(userID)
	defer cancel()
	keep := activityEventFilter(req)
	return pumpStream(ctx, events,
		func() error { return stream.Send(&v1.ActivityEvent{Ts: timestamppb.Now()}) },
		func(ev domain.ActivityEvent) error { return forwardActivityEvent(stream, ev, keep) })
}

// forwardActivityEvent sends ev when it passes the filter.
func forwardActivityEvent(stream activityEventSender, ev domain.ActivityEvent, keep func(domain.ActivityEvent) bool) error {
	if !keep(ev) {
		return nil
	}
	return stream.Send(activityEventToProto(ev))
}

// activityEventFilter keeps the requested types (all when none are given).
func activityEventFilter(req *v1.StreamActivityEventsRequest) func(domain.ActivityEvent) bool {
	types := map[v1.ActivityEventType]bool{}
	for _, t := range req.GetTypes() {
		types[t] = true
	}
	return func(ev domain.ActivityEvent) bool {
		return len(types) == 0 || types[activityEventTypeToProto(ev.Type)]
	}
}

func activityEventToProto(ev domain.ActivityEvent) *v1.ActivityEvent {
	out := &v1.ActivityEvent{
		Type:       activityEventTypeToProto(ev.Type),
		Ts:         timestamppb.New(ev.At),
		I18NKey:    ev.Key,
		I18NParams: ev.Params,
		Progress:   ev.Progress,
	}
	if ev.ActivityID != (domain.ActivityID{}) {
		out.ActivityId = ev.ActivityID.String()
	}
	if ev.SourceID != (domain.SourceID{}) {
		out.ActivitySourceId = ev.SourceID.String()
	}
	if ev.ItemsDone != nil {
		v := int32(*ev.ItemsDone) //nolint:gosec // G115: item counts are small
		out.ItemsDone = &v
	}
	if ev.ItemsTotal != nil {
		v := int32(*ev.ItemsTotal) //nolint:gosec // G115: item counts are small
		out.ItemsTotal = &v
	}
	return out
}

var activityEventTypes = map[domain.ActivityEventType]v1.ActivityEventType{
	domain.ActivityEventImportStarted:     v1.ActivityEventType_ACTIVITY_EVENT_TYPE_IMPORT_STARTED,
	domain.ActivityEventImportProgress:    v1.ActivityEventType_ACTIVITY_EVENT_TYPE_IMPORT_PROGRESS,
	domain.ActivityEventImportCompleted:   v1.ActivityEventType_ACTIVITY_EVENT_TYPE_IMPORT_COMPLETED,
	domain.ActivityEventImportFailed:      v1.ActivityEventType_ACTIVITY_EVENT_TYPE_IMPORT_FAILED,
	domain.ActivityEventActivityCreated:   v1.ActivityEventType_ACTIVITY_EVENT_TYPE_ACTIVITY_CREATED,
	domain.ActivityEventActivityUpdated:   v1.ActivityEventType_ACTIVITY_EVENT_TYPE_ACTIVITY_UPDATED,
	domain.ActivityEventActivityDeleted:   v1.ActivityEventType_ACTIVITY_EVENT_TYPE_ACTIVITY_DELETED,
	domain.ActivityEventSourceAttached:    v1.ActivityEventType_ACTIVITY_EVENT_TYPE_SOURCE_ATTACHED,
	domain.ActivityEventSourceDetached:    v1.ActivityEventType_ACTIVITY_EVENT_TYPE_SOURCE_DETACHED,
	domain.ActivityEventReimportStarted:   v1.ActivityEventType_ACTIVITY_EVENT_TYPE_REIMPORT_STARTED,
	domain.ActivityEventReimportCompleted: v1.ActivityEventType_ACTIVITY_EVENT_TYPE_REIMPORT_COMPLETED,
	domain.ActivityEventReimportFailed:    v1.ActivityEventType_ACTIVITY_EVENT_TYPE_REIMPORT_FAILED,
	domain.ActivityEventNewPR:             v1.ActivityEventType_ACTIVITY_EVENT_TYPE_NEW_PR,
	domain.ActivityEventSegmentPR:         v1.ActivityEventType_ACTIVITY_EVENT_TYPE_SEGMENT_PR,
}

func activityEventTypeToProto(t domain.ActivityEventType) v1.ActivityEventType {
	return activityEventTypes[t]
}
