package instance

import (
	"context"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// HealthState grades a component.
type HealthState string

const (
	Healthy   HealthState = "healthy"
	Degraded  HealthState = "degraded"
	Unhealthy HealthState = "unhealthy"
)

// Component is one subsystem's state with a human line and measurements.
type Component struct {
	State        HealthState
	Detail       string
	Measurements map[string]float64
}

// WorkerCounts is the fleet at a glance.
type WorkerCounts struct{ Healthy, Degraded, Offline int }

// Report is the system health summary.
type Report struct {
	Overall                          HealthState
	Database, Queue, Storage, Email  Component
	Workers                          WorkerCounts
	JobsQueued, JobsRunning, JobsDLQ int
	CheckedAt                        time.Time
}

// Health probes the subsystems. Every probe is optional: a nil one reports
// the component as not configured.
type Health struct {
	PingDB      func(ctx context.Context) error
	QueueOK     func() (connected bool, detail string)
	Blobs       port.BlobStore
	Mail        port.EmailSender
	Workers     func(ctx context.Context) WorkerCounts
	Queue       port.ImportQueueRepo
	DeadLetters port.DeadLetterRepo
	Now         func() time.Time
}

func (h *Health) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now().UTC()
}

// Check runs every probe with a short deadline and grades the whole.
func (h *Health) Check(ctx context.Context) Report {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r := Report{CheckedAt: h.now()}
	r.Database = h.checkDB(ctx)
	r.Queue = h.checkQueue()
	r.Storage = h.checkStorage(ctx)
	r.Email = Component{State: Healthy, Detail: "not configured"}
	if h.Mail != nil {
		r.Email.Detail = "configured"
	}
	if h.Workers != nil {
		r.Workers = h.Workers(ctx)
	}
	if h.Queue != nil {
		if counts, err := h.Queue.CountAllByStatus(ctx, port.ImportQueueFilter{}); err == nil {
			r.JobsQueued, r.JobsRunning = counts[domain.ImportStatusPending], counts[domain.ImportStatusInProgress]
		}
	}
	if h.DeadLetters != nil {
		r.JobsDLQ, _ = h.DeadLetters.CountOpen(ctx)
	}
	r.Overall = overall(r)
	return r
}

func (h *Health) checkDB(ctx context.Context) Component {
	if h.PingDB == nil {
		return Component{State: Unhealthy, Detail: "no database"}
	}
	start := time.Now()
	if err := h.PingDB(ctx); err != nil {
		return Component{State: Unhealthy, Detail: "ping failed: " + err.Error()}
	}
	ms := float64(time.Since(start).Microseconds()) / 1000
	return Component{State: Healthy, Detail: fmt.Sprintf("ping %.1f ms", ms), Measurements: map[string]float64{"ping_ms": ms}}
}

func (h *Health) checkQueue() Component {
	if h.QueueOK == nil {
		return Component{State: Degraded, Detail: "message bus not configured"}
	}
	ok, detail := h.QueueOK()
	if !ok {
		return Component{State: Unhealthy, Detail: detail}
	}
	return Component{State: Healthy, Detail: detail}
}

func (h *Health) checkStorage(ctx context.Context) Component {
	if h.Blobs == nil {
		return Component{State: Degraded, Detail: "object storage not configured"}
	}
	start := time.Now()
	if err := h.Blobs.Healthy(ctx); err != nil {
		return Component{State: Unhealthy, Detail: "probe failed: " + err.Error()}
	}
	ms := float64(time.Since(start).Microseconds()) / 1000
	return Component{State: Healthy, Detail: fmt.Sprintf("probe %.1f ms", ms), Measurements: map[string]float64{"probe_ms": ms}}
}

// overall is the worst component state; a worker fleet with nothing
// healthy but something offline degrades the instance.
func overall(r Report) HealthState {
	worst := Healthy
	for _, c := range []Component{r.Database, r.Queue, r.Storage, r.Email} {
		if rank(c.State) > rank(worst) {
			worst = c.State
		}
	}
	if worst == Healthy && r.Workers.Healthy == 0 && (r.Workers.Degraded > 0 || r.Workers.Offline > 0) {
		worst = Degraded
	}
	if worst == Healthy && r.JobsDLQ > 0 {
		worst = Degraded
	}
	return worst
}

func rank(s HealthState) int {
	switch s {
	case Degraded:
		return 2
	case Unhealthy:
		return 3
	}
	return 1
}
