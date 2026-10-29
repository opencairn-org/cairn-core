package port

import "github.com/opencairn-org/cairn-core/internal/domain"

// AdminEventSink receives operator events for fan-out to open admin
// streams. Publishing is best-effort and never blocks the caller.
type AdminEventSink interface {
	Publish(ev domain.AdminEvent)
}
