package domain

import (
	"errors"
	"strings"
	"time"
)

// WorkerPool is a worker identity: every instance with one name for one
// provider (docs/adr/0006). The name is permanent and bound to one package;
// a different implementation needs a new name.
type WorkerPool struct {
	Provider    string
	Name        string
	Package     string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// ErrPoolPackageChanged: a worker reported another package under a known
// pool name.
var ErrPoolPackageChanged = errors.New("worker pool name is bound to another package")

// ResultSubjectPool reads the pool token of a result subject,
// cairn.results.<job>.<provider>.<name>[.<account>]; "" for the pre-0.5
// provider-scoped form. ok is false for a subject that is not a result.
func ResultSubjectPool(subject string) (job, provider, pool string, ok bool) {
	p := strings.Split(subject, ".")
	if len(p) < 4 || p[0] != "cairn" || p[1] != "results" {
		return "", "", "", false
	}
	job, provider = p[2], p[3]
	switch {
	case job == "reconcile" && len(p) == 6:
		pool = p[4]
	case job != "reconcile" && len(p) == 5:
		pool = p[4]
	}
	return job, provider, pool, true
}
