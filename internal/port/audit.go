package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// AuditRepo persists the append-only audit trail (audit_entries).
type AuditRepo interface {
	// Record appends one entry; the id and timestamp are assigned when zero.
	Record(ctx context.Context, e domain.AuditEntry) error
	// List returns entries newest first with the filter applied.
	List(ctx context.Context, f domain.AuditFilter) ([]domain.AuditEntry, error)
}
