// Package owndata reads a user's own data for agents acting on the user's
// behalf (the MCP server): owner-only, never a projection for others.
package owndata

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Recent-list bounds.
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// Reader reads the caller's own activities, records and profile.
type Reader struct {
	Activities  port.ActivityRepo
	BestEfforts port.BestEffortRepo
	Users       port.UserRepo
}

// Recent lists the user's newest activities; limit is clamped to
// 1..MaxLimit, DefaultLimit when unset.
func (r *Reader) Recent(ctx context.Context, userID domain.UserID, limit int) ([]domain.Activity, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	return r.Activities.ListRecentActivitiesForUser(ctx, userID, limit)
}

// Activity loads one of the user's activities; domain.ErrNotFound for
// anyone else's.
func (r *Reader) Activity(ctx context.Context, userID domain.UserID, id domain.ActivityID) (domain.Activity, error) {
	a, err := r.Activities.GetActivity(ctx, id)
	if err != nil || a.UserID != userID {
		return domain.Activity{}, domain.ErrNotFound
	}
	return a, nil
}

// Totals are the user's lifetime totals.
func (r *Reader) Totals(ctx context.Context, userID domain.UserID) (port.ActivityTotalsResult, error) {
	return r.Activities.ActivityTotals(ctx, userID)
}

// PersonalRecords are the user's current all-time bests.
func (r *Reader) PersonalRecords(ctx context.Context, userID domain.UserID) ([]domain.PersonalRecord, error) {
	return r.BestEfforts.ListPersonalRecords(ctx, userID)
}

// Profile is the user's account.
func (r *Reader) Profile(ctx context.Context, userID domain.UserID) (domain.User, error) {
	return r.Users.GetUser(ctx, userID)
}
