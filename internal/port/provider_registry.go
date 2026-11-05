package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ProviderRegistry stores the provider meta workers declare in their
// manifests. A provider is bound to the first package that published its
// meta; another package's meta is refused with domain.ErrProviderPackageChanged
// until an admin deletes the row.
type ProviderRegistry interface {
	Get(ctx context.Context, id string) (domain.ProviderMeta, error) // domain.ErrNotFound
	List(ctx context.Context) ([]domain.ProviderMeta, error)
	Upsert(ctx context.Context, meta domain.ProviderMeta, pkg string) error
	Delete(ctx context.Context, id string) error
}
