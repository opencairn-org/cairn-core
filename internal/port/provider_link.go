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

// CodeExchange is what the callback brought back for the token exchange.
type CodeExchange struct {
	Code         string
	RedirectURI  string
	CodeVerifier string // PKCE, "" when the provider does not use it
}

// ProviderCodeExchanger trades a provider's OAuth authorization code for
// tokens with the connection's app credentials, through the provider's
// worker pool. It returns the token state and the provider account id (""
// when the provider does not name one).
type ProviderCodeExchanger interface {
	ExchangeCode(ctx context.Context, provider, pool string, conn domain.UserProviderConfig, in CodeExchange) (TokenState, string, error)
}
