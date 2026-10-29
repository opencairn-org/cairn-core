package domain

import (
	"time"

	"github.com/google/uuid"
)

// AdminEventType names an operator-relevant occurrence; the values mirror
// cairn.v1.AdminEventType.
type AdminEventType string

const (
	AdminEventWorkerRegistered     AdminEventType = "worker_registered"
	AdminEventWorkerDeregistered   AdminEventType = "worker_deregistered"
	AdminEventWorkerOffline        AdminEventType = "worker_offline"
	AdminEventWorkerDegraded       AdminEventType = "worker_degraded"
	AdminEventProviderConflict     AdminEventType = "provider_conflict_detected"
	AdminEventJobDeadLettered      AdminEventType = "job_dead_lettered"
	AdminEventUserCreated          AdminEventType = "user_created"
	AdminEventUserSuspended        AdminEventType = "user_suspended"
	AdminEventAuthFloodDetected    AdminEventType = "auth_flood_detected"
	AdminEventSystemHealthDegraded AdminEventType = "system_health_degraded"
)

// AdminEvent is one live item on the admin channel (StreamAdminEvents).
type AdminEvent struct {
	ID         string
	Type       AdminEventType
	Severity   AuditSeverity
	At         time.Time
	Key        string
	Params     map[string]string
	UserID     *UserID
	WorkerName string
	JobID      string
}

// NewAdminEvent builds an event stamped now with a fresh id.
func NewAdminEvent(t AdminEventType, severity AuditSeverity, key string) AdminEvent {
	return AdminEvent{ID: uuid.NewString(), Type: t, Severity: severity, At: time.Now().UTC(), Key: key, Params: map[string]string{}}
}

// With adds one parameter and returns the event for chaining.
func (e AdminEvent) With(k, v string) AdminEvent {
	if e.Params == nil {
		e.Params = map[string]string{}
	}
	e.Params[k] = v
	return e
}

// AboutUser tags the event with the user it concerns.
func (e AdminEvent) AboutUser(id UserID) AdminEvent {
	e.UserID = &id
	return e.With("user_id", id.String())
}

// AboutWorker tags the event with a worker name.
func (e AdminEvent) AboutWorker(name string) AdminEvent {
	e.WorkerName = name
	return e.With("worker", name)
}

// AboutJob tags the event with a job id.
func (e AdminEvent) AboutJob(id string) AdminEvent {
	e.JobID = id
	return e.With("job_id", id)
}
