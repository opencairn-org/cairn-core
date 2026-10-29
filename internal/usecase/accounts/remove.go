// Package accounts holds the lifecycle actions on a provider account that
// go beyond a column flip: removing it with its sources detached.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

// DetachReason marks sources detached by an account removal.
const DetachReason = "account_removed"

// Remover deletes an external account. Its sources are detached first so
// the activities keep their merged data; activities left without a live
// source are soft-deleted only when the caller asks for it.
type Remover struct {
	Accounts   port.ExternalAccountRepo
	Activities port.ActivityRepo
	// Recompute re-merges an activity that still has other sources; nil skips it.
	Recompute func(ctx context.Context, id domain.ActivityID) error
	Audit     *audit.Recorder
	Now       func() time.Time
}

// Result counts what the removal touched.
type Result struct {
	DetachedSources   int
	DeletedActivities int
	RecomputeFailures int
}

func (r *Remover) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

// Remove detaches the account's sources, settles the affected activities
// and deletes the account row. The caller has already checked ownership.
func (r *Remover) Remove(ctx context.Context, actor audit.Actor, acct domain.ExternalAccount, deleteExclusive bool) (Result, error) {
	if r == nil || r.Accounts == nil || r.Activities == nil {
		return Result{}, port.NewActionError(port.ActionErrorUnavailable, "account removal is not available")
	}
	sources, err := r.Activities.ListSourcesForAccount(ctx, acct.ID)
	if err != nil {
		return Result{}, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("list sources: %v", err))
	}
	now := r.now()
	var res Result
	touched := map[domain.ActivityID]bool{}
	for _, s := range sources {
		if err := r.Activities.DetachSource(ctx, s.ID, DetachReason, now); err != nil {
			return res, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("detach source %s: %v", s.ID, err))
		}
		res.DetachedSources++
		touched[s.ActivityID] = true
	}
	for id := range touched {
		if err := r.settle(ctx, id, deleteExclusive, now, &res); err != nil {
			return res, err
		}
	}
	if err := r.Accounts.DeleteAccount(ctx, acct.ID); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return res, port.NewActionError(port.ActionErrorNotFound, "account not found")
		}
		return res, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("delete account: %v", err))
	}
	r.Audit.High(ctx, actor, domain.AuditExternalAccountRemoved, "external_account", acct.ID.String(), map[string]string{
		"provider": acct.Provider, "detached_sources": strconv.Itoa(res.DetachedSources), "deleted_activities": strconv.Itoa(res.DeletedActivities)})
	return res, nil
}

// settle re-merges an activity that still has sources, or soft-deletes an
// orphaned one when asked; an orphan kept alive keeps its last merged state.
func (r *Remover) settle(ctx context.Context, id domain.ActivityID, deleteExclusive bool, now time.Time, res *Result) error {
	remaining, err := r.Activities.ListSourcesForActivity(ctx, id)
	if err != nil {
		return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("list sources of %s: %v", id, err))
	}
	if len(remaining) > 0 {
		if r.Recompute != nil && r.Recompute(ctx, id) != nil {
			res.RecomputeFailures++
		}
		return nil
	}
	if !deleteExclusive {
		return nil
	}
	if err := r.Activities.SoftDeleteActivity(ctx, id, now); err != nil {
		return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("delete activity %s: %v", id, err))
	}
	res.DeletedActivities++
	return nil
}
