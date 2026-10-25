package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// WorkerRouter picks the named worker pool an external account's jobs are
// published to (docs/adr/0006).
type WorkerRouter interface {
	AccountWorker(ctx context.Context, a domain.ExternalAccount) (string, error)
}
