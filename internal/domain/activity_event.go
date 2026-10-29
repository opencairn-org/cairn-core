package domain

import "time"

// ActivityEventType names what happened to a user's activities; the values
// mirror cairn.v1.ActivityEventType.
type ActivityEventType string

const (
	ActivityEventImportStarted     ActivityEventType = "import_started"
	ActivityEventImportProgress    ActivityEventType = "import_progress"
	ActivityEventImportCompleted   ActivityEventType = "import_completed"
	ActivityEventImportFailed      ActivityEventType = "import_failed"
	ActivityEventActivityCreated   ActivityEventType = "activity_created"
	ActivityEventActivityUpdated   ActivityEventType = "activity_updated"
	ActivityEventActivityDeleted   ActivityEventType = "activity_deleted"
	ActivityEventSourceAttached    ActivityEventType = "source_attached"
	ActivityEventSourceDetached    ActivityEventType = "source_detached"
	ActivityEventReimportStarted   ActivityEventType = "reimport_started"
	ActivityEventReimportCompleted ActivityEventType = "reimport_completed"
	ActivityEventReimportFailed    ActivityEventType = "reimport_failed"
	ActivityEventNewPR             ActivityEventType = "new_pr"
	ActivityEventSegmentPR         ActivityEventType = "segment_pr"
)

// ActivityEvent is one live event for the owning user (StreamActivityEvents).
// Key and Params let clients render a translated line without a lookup.
type ActivityEvent struct {
	Type       ActivityEventType
	At         time.Time
	UserID     UserID
	ActivityID ActivityID
	SourceID   SourceID
	Key        string
	Params     map[string]string
	Progress   *float64
	ItemsDone  *int
	ItemsTotal *int
}

// NewActivityEvent builds an event stamped now.
func NewActivityEvent(t ActivityEventType, userID UserID, key string) ActivityEvent {
	return ActivityEvent{Type: t, At: time.Now().UTC(), UserID: userID, Key: key, Params: map[string]string{}}
}

// With adds one parameter and returns the event for chaining.
func (e ActivityEvent) With(k, v string) ActivityEvent {
	if e.Params == nil {
		e.Params = map[string]string{}
	}
	e.Params[k] = v
	return e
}

// For sets the activity (and optionally source) the event is about.
func (e ActivityEvent) For(activityID ActivityID, sourceID SourceID) ActivityEvent {
	e.ActivityID, e.SourceID = activityID, sourceID
	return e
}
