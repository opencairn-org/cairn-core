package connect

import (
	"context"

	connectrpc "connectrpc.com/connect/v2"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	notificationuc "github.com/opencairn-org/cairn-core/internal/usecase/notification"
)

// Preference matrix RPCs, twins of GET/PUT /api/notifications/preferences.
// v1 resolves the email channel; every configurable type is returned with
// its effective value (a proto3 bool cannot default to true, so
// include_defaults is accepted but always behaves as set).

func (s *NotificationServer) ListNotificationPreferences(ctx context.Context, _ *v1.ListNotificationPreferencesRequest) (*v1.ListNotificationPreferencesResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Preferences == nil {
		return nil, unavailable("notification preferences")
	}
	cells, err := s.Preferences.Matrix(ctx, userID, domain.NotificationChannelEmail)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListNotificationPreferencesResponse{}
	for _, c := range cells {
		out.Preferences = append(out.Preferences, preferenceToProto(userID, c))
	}
	return out, nil
}

func (s *NotificationServer) UpdateNotificationPreferences(ctx context.Context, req *v1.UpdateNotificationPreferencesRequest) (*v1.UpdateNotificationPreferencesResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Preferences == nil {
		return nil, unavailable("notification preferences")
	}
	for _, p := range req.GetPreferences() {
		t, ok := notificationTypeFromProto(p.GetEventType())
		if !ok {
			return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "unknown event_type")
		}
		if err := s.Preferences.Set(ctx, userID, t, notificationChannelFromProto(p.GetChannel()), p.GetEnabled()); err != nil {
			return nil, actionErr(err)
		}
	}
	cells, err := s.Preferences.Matrix(ctx, userID, domain.NotificationChannelEmail)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.UpdateNotificationPreferencesResponse{}
	for _, c := range cells {
		out.Preferences = append(out.Preferences, preferenceToProto(userID, c))
	}
	return out, nil
}

func preferenceToProto(userID domain.UserID, c notificationuc.Resolved) *v1.NotificationPreference {
	return &v1.NotificationPreference{UserId: userID.String(), EventType: notificationTypeToProto(c.Type), Channel: notificationChannelToProto(c.Channel),
		Enabled: c.Enabled, MinSeverity: v1.NotificationSeverity_NOTIFICATION_SEVERITY_INFO}
}

// notificationTypeFromProto inverts notificationTypeToProto over the
// configurable types.
func notificationTypeFromProto(t v1.NotificationEventType) (domain.NotificationType, bool) {
	for _, known := range domain.UserFacingNotificationTypes {
		if notificationTypeToProto(known) == t {
			return known, true
		}
	}
	return domain.NotificationTypeUnspecified, false
}

var notificationChannels = map[domain.NotificationChannel]v1.NotificationChannel{
	domain.NotificationChannelInApp: v1.NotificationChannel_NOTIFICATION_CHANNEL_IN_APP, domain.NotificationChannelEmail: v1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL,
	domain.NotificationChannelWebhook: v1.NotificationChannel_NOTIFICATION_CHANNEL_WEBHOOK, domain.NotificationChannelPush: v1.NotificationChannel_NOTIFICATION_CHANNEL_PUSH,
}

func notificationChannelToProto(ch domain.NotificationChannel) v1.NotificationChannel {
	return notificationChannels[ch]
}

func notificationChannelFromProto(ch v1.NotificationChannel) domain.NotificationChannel {
	for d, p := range notificationChannels {
		if p == ch {
			return d
		}
	}
	return ""
}
