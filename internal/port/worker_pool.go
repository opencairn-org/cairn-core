package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// WorkerPoolRepo keeps the known worker pools (docs/adr/0006).
type WorkerPoolRepo interface {
	// Upsert records a pool, refreshing last_seen; a known name with another
	// package is left as is and reported as domain.ErrPoolPackageChanged.
	Upsert(ctx context.Context, p domain.WorkerPool) error
	Get(ctx context.Context, provider, name string) (domain.WorkerPool, error)
	ListForProvider(ctx context.Context, provider string) ([]domain.WorkerPool, error)
	ListAll(ctx context.Context) ([]domain.WorkerPool, error)
	// Retire hides a pool from routing and pickers; its name stays bound.
	Retire(ctx context.Context, provider, name string) error
}
