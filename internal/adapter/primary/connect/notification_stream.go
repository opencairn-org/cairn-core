package connect

import (
	"context"
	"fmt"
	"time"

	connectrpc "connectrpc.com/connect/v2"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	notificationuc "github.com/opencairn-org/cairn-core/internal/usecase/notification"
)

// streamHeartbeat is how often an idle stream sends a keep-alive event.
const streamHeartbeat = 30 * time.Second

// CountUnreadNotifications is the badge number.
func (s *NotificationServer) CountUnreadNotifications(ctx context.Context, _ *v1.CountUnreadNotificationsRequest) (*v1.CountUnreadNotificationsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.Notifications.CountUnreadForUser(ctx, userID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("count unread: %w", err))
	}
	return &v1.CountUnreadNotificationsResponse{Count: connectClampInt32(n)}, nil
}

// StreamNotifications pushes the caller's new notifications as they are
// saved, with a heartbeat (an event with an empty id) every 30 s while idle.
// It carries only events created after the stream opened; ListNotifications
// backfills. since_event_id is accepted but not used for replay.
func (s *NotificationServer) StreamNotifications(ctx context.Context, req *v1.StreamNotificationsRequest, stream cairnv1connect.NotificationServiceStreamNotificationsServerStream) error {
	return s.streamNotifications(ctx, req, stream)
}

// eventSender is the send side of the stream (the generated stream, or a
// test double).
type eventSender interface {
	Send(*v1.NotificationEvent) error
}

func (s *NotificationServer) streamNotifications(ctx context.Context, req *v1.StreamNotificationsRequest, stream eventSender) error {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return err
	}
	if s.Hub == nil {
		return connectrpc.NewError(connectrpc.CodeUnavailable, "live notifications are not available on this instance")
	}
	events, cancel := s.Hub.Subscribe(userID)
	defer cancel()
	keep := streamFilter(req)
	ticker := time.NewTicker(streamHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := stream.Send(&v1.NotificationEvent{UserId: userID.String()}); err != nil {
				return err
			}
		case batch, ok := <-events:
			if !ok {
				return nil
			}
			if err := forwardBatch(stream, batch, keep); err != nil {
				return err
			}
		}
	}
}

// forwardBatch sends the events of one batch that pass the filter.
func forwardBatch(stream eventSender, batch []domain.Notification, keep func(domain.Notification) bool) error {
	for _, n := range batch {
		if !keep(n) {
			continue
		}
		if err := stream.Send(notificationToProto(n)); err != nil {
			return err
		}
	}
	return nil
}

// streamFilter builds the type / min-severity predicate of a stream request.
func streamFilter(req *v1.StreamNotificationsRequest) func(domain.Notification) bool {
	types := map[v1.NotificationEventType]bool{}
	for _, t := range req.GetTypes() {
		types[t] = true
	}
	minSev := req.GetMinSeverity()
	return func(n domain.Notification) bool {
		if len(types) > 0 && !types[notificationTypeToProto(n.Type)] {
			return false
		}
		return minSev == v1.NotificationSeverity_NOTIFICATION_SEVERITY_UNSPECIFIED || notificationSeverityToProto(n.Severity) >= minSev
	}
}

// Hub-backed streaming is optional; the field is set by package main.
var _ = (*notificationuc.Hub)(nil)
