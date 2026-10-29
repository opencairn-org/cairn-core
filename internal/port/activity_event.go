package port

import "github.com/opencairn-org/cairn-core/internal/domain"

// ActivityEventSink receives live activity events for fan-out to open
// streams. Publishing is best-effort and never blocks the caller.
type ActivityEventSink interface {
	Publish(ev domain.ActivityEvent)
}
