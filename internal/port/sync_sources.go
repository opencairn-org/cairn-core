package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// AccountSourceRepo is the per-connection source bookkeeping behind full
// sync, bulk re-parse and moving a connection to another worker pool.
type AccountSourceRepo interface {
	// ExistingExternalIDs is the set of provider ids already imported.
	ExistingExternalIDs(ctx context.Context, provider string, accountID domain.ExternalAccountID) (map[string]struct{}, error)
	// CountSourcesOutOfDate counts update_available sources per reason.
	CountSourcesOutOfDate(ctx context.Context, accountID domain.ExternalAccountID) (map[string]int, error)
	// ListSourcesOutOfDate lists update_available sources of one reason.
	ListSourcesOutOfDate(ctx context.Context, accountID domain.ExternalAccountID, reason string, limit int) ([]domain.ActivitySource, error)
	// ListSourceWorkersForAccount lists the pools that imported the live sources.
	ListSourceWorkersForAccount(ctx context.Context, accountID domain.ExternalAccountID) ([]domain.SourceWorker, error)
	// MarkSourcesPoolChanged flags sources imported by another package for a re-fetch.
	MarkSourcesPoolChanged(ctx context.Context, accountID domain.ExternalAccountID, newPool string) (int, error)
}
