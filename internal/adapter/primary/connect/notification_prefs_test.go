package connect

import (
	"context"
	"testing"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	notificationuc "github.com/opencairn-org/cairn-core/internal/usecase/notification"
)

type prefRows struct {
	rows []domain.NotificationPreference
}

func (s *prefRows) ListForUser(context.Context, domain.UserID) ([]domain.NotificationPreference, error) {
	return s.rows, nil
}
func (s *prefRows) Upsert(_ context.Context, p domain.NotificationPreference) error {
	s.rows = append(s.rows, p)
	return nil
}

func TestNotificationPreferencesRPCs(t *testing.T) {
	me := domain.UserID(uuid.New())
	ctx := context.WithValue(t.Context(), ctxKeyUserID, me)
	s := &NotificationServer{}
	if _, err := s.ListNotificationPreferences(ctx, &v1.ListNotificationPreferencesRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("unwired: %v", err)
	}
	store := &prefRows{}
	s.Preferences = &notificationuc.Preferences{Repo: store}
	if _, err := s.ListNotificationPreferences(t.Context(), &v1.ListNotificationPreferencesRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
	list, err := s.ListNotificationPreferences(ctx, &v1.ListNotificationPreferencesRequest{IncludeDefaults: true})
	if err != nil || len(list.GetPreferences()) != len(domain.UserFacingNotificationTypes) || list.GetPreferences()[0].GetChannel() != v1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL {
		t.Fatalf("list: %v %v", list, err)
	}
	target := list.GetPreferences()[0]
	upd, err := s.UpdateNotificationPreferences(ctx, &v1.UpdateNotificationPreferencesRequest{Preferences: []*v1.NotificationPreference{
		{EventType: target.GetEventType(), Channel: v1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL, Enabled: !target.GetEnabled()}}})
	if err != nil || len(store.rows) != 1 || upd.GetPreferences()[0].GetEnabled() == target.GetEnabled() || upd.GetPreferences()[0].GetUserId() != me.String() {
		t.Fatalf("update: %v %v %+v", upd, err, store.rows)
	}
	_, err = s.UpdateNotificationPreferences(ctx, &v1.UpdateNotificationPreferencesRequest{Preferences: []*v1.NotificationPreference{
		{EventType: target.GetEventType(), Channel: v1.NotificationChannel_NOTIFICATION_CHANNEL_WEBHOOK, Enabled: true}}})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	_, err = s.UpdateNotificationPreferences(ctx, &v1.UpdateNotificationPreferencesRequest{Preferences: []*v1.NotificationPreference{
		{EventType: v1.NotificationEventType_NOTIFICATION_EVENT_TYPE_SECURITY_PAT_CREATED, Channel: v1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL}}})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
}
