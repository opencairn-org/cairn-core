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
	// ErrPoolTaken: another connection of the same provider account already
	// uses that pool (one connection per account and pool).
	ErrPoolTaken = errors.New("another connection of this account already uses that worker")
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

// ResolveConnectPool picks the pool a new or re-made connection is created
// for (docs/adr/0006): the user's explicit choice (must be live), else the
// pool this connection already used for the account, else the provider's
// only live pool. No live pool leaves it unassigned (the router assigns it
// later); several without a choice is ambiguous.
func ResolveConnectPool(requested, existing string, live []string) (string, error) {
	names := slices.Compact(slices.Sorted(slices.Values(live)))
	switch {
	case requested != "":
		if !slices.Contains(names, requested) {
			return "", ErrNoWorker
		}
		return requested, nil
	case existing != "":
		return existing, nil
	case len(names) == 0:
		return "", nil
	case len(names) == 1:
		return names[0], nil
	}
	return "", ErrWorkerAmbiguous
}
