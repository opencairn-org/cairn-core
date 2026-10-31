package notification

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// NotifyWorkerOffline broadcasts a "connector went offline" notification to
// every admin when a worker stops heartbeating. Workers are instance-level (not
// per-user), so there's no single owner — the recipients are the instance's
// admins.
//
// Coalescing: DedupKey = "worker_offline:<workerKey>" (no date) so a flapping
// worker folds into one notification per admin within the 24h window.
type NotifyWorkerOffline struct {
	users         port.UserReader
	notifications port.NotificationRepo
	tx            port.TxManager
	deliver       *DeliverNotifications

	now   func() time.Time
	newID func() uuid.UUID
}

func NewNotifyWorkerOffline(
	users port.UserReader,
	notifications port.NotificationRepo,
	tx port.TxManager,
	deliver *DeliverNotifications,
	now func() time.Time,
	newID func() uuid.UUID,
) *NotifyWorkerOffline {
	now, newID = notificationClockDefaults(now, newID)
	return &NotifyWorkerOffline{
		users:         users,
		notifications: notifications,
		tx:            tx,
		deliver:       deliver,
		now:           now,
		newID:         newID,
	}
}

// Execute creates + delivers one worker-offline notification per active admin.
// workerKey is the stable worker identity (provider-scoped name); displayName
// is what the operator sees.
func (uc *NotifyWorkerOffline) Execute(ctx context.Context, workerKey, displayName string) error {
	admins, err := uc.users.ListAdmins(ctx)
	if err != nil {
		return fmt.Errorf("list admins: %w", err)
	}
	if len(admins) == 0 {
		return nil
	}

	now := uc.now()
	dedup := "worker_offline:" + workerKey
	batch := make([]domain.Notification, 0, len(admins))
	for _, a := range admins {
		batch = append(batch, domain.Notification{
			ID:           domain.NotificationID(uc.newID()),
			UserID:       a.ID,
			Type:         domain.NotificationTypeWorkerOffline,
			Severity:     domain.NotificationSeverityWarn,
			TitleI18nKey: "notification.worker_offline.title",
			BodyI18nKey:  "notification.worker_offline.body",
			I18nParams:   map[string]string{"worker": displayName, "worker_key": workerKey},
			WorkerName:   displayName,
			DedupKey:     dedup,
			CreatedAt:    now,
			UpdatedAt:    now,
		})
	}

	if err := uc.tx.InTx(ctx, func(ctx context.Context) error {
		return uc.notifications.SaveNotifications(ctx, batch)
	}); err != nil {
		return fmt.Errorf("persist worker-offline notifications: %w", err)
	}
	uc.deliver.Execute(ctx, batch)
	return nil
}
