package domain

import (
	"crypto/sha256"
	"encoding/hex"
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
	RetiredAt   *time.Time
}

// AppID identifies a provider app (OAuth client id) without exposing it:
// the first 12 hex digits of its SHA-256. Rate-limit budgets are per app.
func AppID(clientID string) string {
	if clientID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(clientID))
	return hex.EncodeToString(sum[:])[:12]
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
