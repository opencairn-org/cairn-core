package domain

import (
	"errors"
	"slices"
)

var (
	// ErrNoWorker: the account has no assigned pool and none is online.
	ErrNoWorker = errors.New("no worker online for this provider")
	// ErrWorkerAmbiguous: several pools serve the provider and the account
	// is not assigned to one.
	ErrWorkerAmbiguous = errors.New("several workers serve this provider; assign one to the account")
)

// ResolveAccountWorker picks the named pool an account's jobs go to
// (docs/adr/0006). An assignment wins, online or not (the job waits in the
// queue). Without one, the provider's only live pool is used and should be
// persisted as the assignment.
func ResolveAccountWorker(assigned string, live []string) (name string, persist bool, err error) {
	if assigned != "" {
		return assigned, false, nil
	}
	names := slices.Compact(slices.Sorted(slices.Values(live)))
	switch len(names) {
	case 0:
		return "", false, ErrNoWorker
	case 1:
		return names[0], true, nil
	}
	return "", false, ErrWorkerAmbiguous
}
