package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// SyncInsight is the live, bus- and registry-backed side of the sync status:
// provider budgets from the rate-limit KV, worker pools from the registry
// plus presence, the provider registry and the background discovery.
// Implemented in package main; injected into the read use cases so REST and
// Connect render the same rows.
type SyncInsight interface {
	// Budget is the live API budget of the account's provider app.
	Budget(ctx context.Context, a domain.ExternalAccount) domain.RateBudget
	// Pools lists a provider's known, non-retired pools.
	Pools(ctx context.Context, provider string) []domain.WorkerPoolInfo
	// Providers lists the providers the instance can connect.
	Providers() []domain.ProviderInfo
	// ActivityURL is the provider page of an activity, "" when none.
	ActivityURL(provider, externalID string) string
	// StartFullSync starts a background discovery of the account.
	StartFullSync(a domain.ExternalAccount, skipExisting bool)
}
