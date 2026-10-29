package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// WorkerPresence reports the workers currently online (from the NATS presence
// bucket). Implemented in package main over a short-lived cache; nil when the
// bus is not wired, in which case nothing is live.
type WorkerPresence interface {
	LiveWorkers(ctx context.Context) []domain.LiveWorker
}
