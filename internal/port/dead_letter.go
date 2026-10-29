package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// DeadLetterFilter narrows a dead-letter listing.
type DeadLetterFilter struct {
	Stream            string // empty = all
	Subject           string // empty = all
	IncludeReplayed   bool
	Limit             int
	BeforeFirstSeenAt time.Time // cursor pagination
}

// DeadLetterRepo persists messages JetStream gave up on (dead_lettered_jobs).
type DeadLetterRepo interface {
	Capture(ctx context.Context, j domain.DeadLetteredJob) error
	List(ctx context.Context, f DeadLetterFilter) ([]domain.DeadLetteredJob, error)
	Get(ctx context.Context, id domain.DeadLetteredJobID) (domain.DeadLetteredJob, error)
	MarkReplayed(ctx context.Context, id domain.DeadLetteredJobID, by *domain.UserID, at time.Time) error
	// CountOpen counts rows not yet replayed.
	CountOpen(ctx context.Context) (int, error)
}
