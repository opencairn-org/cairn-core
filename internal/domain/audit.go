package domain

import "time"

// AuditSeverity grades an audit entry; mirrors cairn.v1.AuditSeverity.
type AuditSeverity string

const (
	AuditInfo AuditSeverity = "info"
	AuditWarn AuditSeverity = "warn"
	AuditHigh AuditSeverity = "high"
)

// Rank orders severities for a minimum-severity filter.
func (s AuditSeverity) Rank() int {
	switch s {
	case AuditWarn:
		return 2
	case AuditHigh:
		return 3
	}
	return 1
}

// AuditEntry is one immutable line of the instance's audit trail.
type AuditEntry struct {
	ID            AuditEntryID
	At            time.Time
	ActorUserID   *UserID
	ActorUsername string
	ActorPATID    *PATID
	IPAddress     string
	UserAgent     string
	// Action is a stable "resource.verb" identifier, e.g. "user.suspended".
	Action       string
	Severity     AuditSeverity
	ResourceType string
	ResourceID   string
	Diff         []FieldChange
	Metadata     map[string]string
}

// AuditFilter narrows ListAuditEntries.
type AuditFilter struct {
	From, To      time.Time
	ActorUserID   *UserID
	ResourceType  string
	ResourceID    string
	Actions       []string
	MinSeverity   AuditSeverity
	Query         string
	Limit, Offset int
}

// Audit action names recorded by core. Keep them stable: clients and
// operators filter on them.
const (
	AuditUserCreated            = "user.created"
	AuditUserUpdated            = "user.updated"
	AuditUserRoleChanged        = "user.role_changed"
	AuditUserStatusChanged      = "user.status_changed"
	AuditUserDeleted            = "user.deleted"
	AuditUserPasswordReset      = "user.password_reset" //nolint:gosec // G101: an action name, not a credential
	AuditUserCredentialsRevoke  = "user.credentials_revoked"
	AuditInviteCreated          = "invite.created"
	AuditInviteRevoked          = "invite.revoked"
	AuditInviteResent           = "invite.resent"
	AuditInstanceSettings       = "instance_settings.updated"
	AuditLoginSucceeded         = "auth.login_succeeded"
	AuditLoginFailed            = "auth.login_failed"
	AuditPasswordChanged        = "auth.password_changed"
	AuditSessionRevoked         = "auth.session_revoked"
	AuditJobRetried             = "job.retried"
	AuditJobCancelled           = "job.cancelled"
	AuditJobRequeued            = "job.requeued"
	AuditMaintenance            = "maintenance.triggered"
	AuditWorkerPoolRetired      = "worker_pool.retired"
	AuditEnrollmentCreated      = "worker_enrollment.created"
	AuditEnrollmentRevoked      = "worker_enrollment.revoked"
	AuditFederationBlocked      = "federation.domain_blocked"
	AuditFederationUnblocked    = "federation.domain_unblocked"
	AuditReportResolved         = "report.resolved"
	AuditActivityHidden         = "activity.hidden"
	AuditQuotaChanged           = "user.quota_changed"
	AuditWebhookTokensRotated   = "webhook_tokens.rotated" //nolint:gosec // G101: an action name, not a credential
	AuditExternalAccountRemoved = "external_account.removed"
)
