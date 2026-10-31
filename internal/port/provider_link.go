package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
)

// PoolDirectory lists the names of a provider's known, unretired worker
// pools (docs/adr/0006).
type PoolDirectory interface {
	KnownPoolNames(ctx context.Context, provider string) []string
}

// PoolCapabilities reports whether a live, compatible instance of a pool can
// read a data type.
type PoolCapabilities interface {
	PoolDeclares(ctx context.Context, provider, pool string, dt capability.DataType) bool
}

// ProviderCodeExchanger trades a provider's OAuth authorization code for
// tokens with the connection's app credentials. It returns the token state
// and the provider account id ("" when the provider does not name one).
type ProviderCodeExchanger interface {
	ExchangeCode(ctx context.Context, tokenURL string, conn domain.UserProviderConfig, code string) (TokenState, string, error)
}
