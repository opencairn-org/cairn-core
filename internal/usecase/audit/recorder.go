// Package audit writes the instance's audit trail. Recording is best-effort
// and never fails the action it describes: a failed write is logged.
package audit

import (
	"context"
	"log/slog"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Actor identifies who performed an action; zero for system actions.
type Actor struct {
	UserID    *domain.UserID
	Username  string
	PATID     *domain.PATID
	IP        string
	UserAgent string
}

// UserActor builds an actor from a user.
func UserActor(u domain.User) Actor {
	id := u.ID
	return Actor{UserID: &id, Username: u.Username}
}

// Recorder appends entries; a nil Recorder is safe to call.
type Recorder struct {
	Repo port.AuditRepo
	Log  *slog.Logger
}

// Record appends one entry; failures are logged, never returned.
func (r *Recorder) Record(ctx context.Context, actor Actor, action string, severity domain.AuditSeverity, resourceType, resourceID string, diff []domain.FieldChange, meta map[string]string) {
	if r == nil || r.Repo == nil {
		return
	}
	err := r.Repo.Record(ctx, domain.AuditEntry{
		ActorUserID: actor.UserID, ActorUsername: actor.Username, ActorPATID: actor.PATID,
		IPAddress: actor.IP, UserAgent: actor.UserAgent,
		Action: action, Severity: severity, ResourceType: resourceType, ResourceID: resourceID,
		Diff: diff, Metadata: meta,
	})
	if err != nil && r.Log != nil {
		r.Log.Warn("audit record failed", "action", action, "error", err)
	}
}

// Info / High are the common severities.
func (r *Recorder) Info(ctx context.Context, actor Actor, action, resourceType, resourceID string, meta map[string]string) {
	r.Record(ctx, actor, action, domain.AuditInfo, resourceType, resourceID, nil, meta)
}

func (r *Recorder) High(ctx context.Context, actor Actor, action, resourceType, resourceID string, meta map[string]string) {
	r.Record(ctx, actor, action, domain.AuditHigh, resourceType, resourceID, nil, meta)
}

// List reads the trail.
func (r *Recorder) List(ctx context.Context, f domain.AuditFilter) ([]domain.AuditEntry, error) {
	if r == nil || r.Repo == nil {
		return nil, port.NewActionError(port.ActionErrorUnavailable, "the audit log is not available on this instance")
	}
	list, err := r.Repo.List(ctx, f)
	if err != nil {
		return nil, port.NewActionError(port.ActionErrorInternal, "list audit entries: "+err.Error())
	}
	return list, nil
}
