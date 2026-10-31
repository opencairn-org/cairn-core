// Package notification holds the notification dispatcher use cases.
// In v1 there's one: DispatchPRNotifications, which fires after the
// segment-match + rank-refresh pipeline and emits one notification per
// new personal-record segment effort.
package notification

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// DispatchPRNotifications looks at the segment efforts a given source
// just produced and writes a notification for every effort whose
// is_personal_record flag is set after the rank-refresh pass.
//
// Coalescing: each notification carries DedupKey =
//
//	"segment_pr:<segment_id>:<UTC-date>"
//
// so multiple PRs on the same segment in the same workout (hill repeats)
// produce one notification with coalesce_count incremented, instead of
// spamming the inbox.
//
// Idempotent: rerunning the dispatcher for the same source produces the
// same dedup keys, and the repo's coalesce-or-insert path handles
// repeated writes gracefully.
type DispatchPRNotifications struct {
	segments      port.SegmentReader
	notifications port.NotificationRepo
	tx            port.TxManager
	// deliver fans notifications out to side-channels (email). Optional — nil
	// when email is disabled or in tests; in-app persistence is unaffected.
	deliver *DeliverNotifications
	// Events receives a live segment-PR event per notification (optional).
	Events port.ActivityEventSink

	now   func() time.Time
	newID func() uuid.UUID
}

func NewDispatchPRNotifications(
	segments port.SegmentReader,
	notifications port.NotificationRepo,
	tx port.TxManager,
	deliver *DeliverNotifications,
	now func() time.Time,
	newID func() uuid.UUID,
) *DispatchPRNotifications {
	now, newID = notificationClockDefaults(now, newID)
	return &DispatchPRNotifications{
		segments:      segments,
		notifications: notifications,
		tx:            tx,
		deliver:       deliver,
		now:           now,
		newID:         newID,
	}
}

// notificationClockDefaults fills nil clock / ID generators with UTC now and UUIDv7.
func notificationClockDefaults(now func() time.Time, newID func() uuid.UUID) (func() time.Time, func() uuid.UUID) {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if newID == nil {
		newID = func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New()
			}
			return id
		}
	}
	return now, newID
}

// Input identifies the activity and the source whose efforts the
// dispatcher should walk. The activity bound lets us pull all efforts
// in one call; the source bound filters down to "this source's
// contribution" (multi-source activities have efforts per source).
type Input struct {
	ActivityID domain.ActivityID
	SourceID   domain.SourceID
}

// Result reports how many notifications the dispatcher wrote.
type Result struct {
	NotificationsCreated int
}

func (uc *DispatchPRNotifications) Execute(ctx context.Context, in Input) (Result, error) {
	efforts, err := uc.segments.ListEffortsForActivity(ctx, in.ActivityID)
	if err != nil {
		return Result{}, fmt.Errorf("list efforts for %s: %w", in.ActivityID, err)
	}

	now := uc.now()
	var notifs []domain.Notification
	for _, e := range efforts {
		if e.ActivitySourceID != in.SourceID {
			continue
		}
		if !e.IsPersonalRecord {
			continue
		}
		notifs = append(notifs, buildPRNotification(e, uc.newID, now))
		if uc.Events != nil {
			uc.Events.Publish(domain.NewActivityEvent(domain.ActivityEventSegmentPR, e.UserID, "activity.segment_pr").
				For(e.ActivityID, e.ActivitySourceID).With("segment_id", e.SegmentID.String()).
				With("elapsed_s", strconv.Itoa(int(e.ElapsedS))))
		}
	}

	if len(notifs) == 0 {
		return Result{}, nil
	}

	err = uc.tx.InTx(ctx, func(ctx context.Context) error {
		return uc.notifications.SaveNotifications(ctx, notifs)
	})
	if err != nil {
		return Result{}, fmt.Errorf("persist notifications: %w", err)
	}

	// Fan out to side-channels (email) outside the tx — best-effort, never
	// fails the durable in-app write.
	uc.deliver.Execute(ctx, notifs)

	return Result{NotificationsCreated: len(notifs)}, nil
}

// buildPRNotification assembles the in-app event row for one effort.
// I18nParams carry the segment_id and elapsed time so the frontend can
// render "Personal record on <segment-name>: 4:32" once it has the
// segment's name from a separate read.
func buildPRNotification(
	e domain.SegmentEffort,
	newID func() uuid.UUID,
	now time.Time,
) domain.Notification {
	notifType := domain.NotificationTypeSegmentPersonalRecord
	if e.IsInstanceRecord {
		// Promote to instance-record type when the same effort is also
		// the fastest across the whole instance. The frontend renders
		// these with stronger emphasis ("KOM").
		notifType = domain.NotificationTypeSegmentInstanceRecord
	}

	dedupKey := fmt.Sprintf("segment_pr:%s:%s",
		e.SegmentID, e.StartTime.UTC().Format("2006-01-02"),
	)

	activityID := e.ActivityID
	segmentID := e.SegmentID
	return domain.Notification{
		ID:           domain.NotificationID(newID()),
		UserID:       e.UserID,
		Type:         notifType,
		Severity:     domain.NotificationSeverityInfo,
		TitleI18nKey: "notification.segment_pr.title",
		BodyI18nKey:  "notification.segment_pr.body",
		I18nParams: map[string]string{
			"segment_id": e.SegmentID.String(),
			"elapsed_s":  fmt.Sprintf("%.1f", e.ElapsedS),
		},
		ActivityID: &activityID,
		SegmentID:  &segmentID,
		DedupKey:   dedupKey,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}
