package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// BlockRepo persists user blocks (migration 36).
type BlockRepo interface {
	Block(ctx context.Context, blocker, blocked domain.UserID) error
	Unblock(ctx context.Context, blocker, blocked domain.UserID) error
	// IsBlocked reports whether `a` has blocked `b` OR `b` has blocked `a`
	// (blocking is mutually isolating).
	IsBlockedEitherWay(ctx context.Context, a, b domain.UserID) (bool, error)
	ListBlocked(ctx context.Context, blocker domain.UserID) ([]domain.UserID, error)
}

// ReportRepo persists content reports for the moderation queue (migration 36).
type ReportRepo interface {
	Create(ctx context.Context, r domain.ContentReport) (domain.ContentReportID, error)
	List(ctx context.Context, status domain.ReportStatus, limit, offset int) ([]domain.ContentReport, error)
	UpdateStatus(ctx context.Context, id domain.ContentReportID, status domain.ReportStatus, reviewer domain.UserID) error
}

// ModerationRepo is admin content actions not tied to a single aggregate.
type ModerationRepo interface {
	SetActivityHidden(ctx context.Context, activityID domain.ActivityID, hidden bool) error
}
