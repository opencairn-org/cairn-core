package dashboard

import (
	"context"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Overview is the landing dashboard's data.
type Overview struct {
	Totals   port.ActivityTotalsResult
	BySport  []port.ActivityFacet
	Recent   []domain.Activity
	ThisWeek port.ActivityTotalsResult
	LastWeek port.ActivityTotalsResult
	// Half-open ranges [From, To) of the two weeks, UTC, Monday start.
	ThisWeekFrom, ThisWeekTo time.Time
	LastWeekFrom, LastWeekTo time.Time
}

// RecentCount is how many newest activities the overview carries.
const RecentCount = 6

// Overviews computes the dashboard overview (GET /api/overview, StatsService.GetOverview).
type Overviews struct {
	activities port.ActivityReader
	now        func() time.Time
}

// NewOverviews wires the repo; now defaults to time.Now.
func NewOverviews(activities port.ActivityReader, now func() time.Time) *Overviews {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Overviews{activities: activities, now: now}
}

// WeekBounds returns the Monday 00:00 UTC that starts the week of now and
// the Monday before it.
func WeekBounds(now time.Time) (thisWeekStart, lastWeekStart time.Time) {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	offset := (int(today.Weekday()) + 6) % 7 // Mon=0 … Sun=6
	thisWeekStart = today.AddDate(0, 0, -offset)
	return thisWeekStart, thisWeekStart.AddDate(0, 0, -7)
}

// Get computes the overview of userID. Only the lifetime totals are required;
// the other parts degrade to empty on error (the dashboard still renders).
func (o *Overviews) Get(ctx context.Context, userID domain.UserID) (Overview, error) {
	totals, err := o.activities.ActivityTotals(ctx, userID)
	if err != nil {
		return Overview{}, fmt.Errorf("activity totals: %w", err)
	}
	out := Overview{Totals: totals}
	out.BySport, _, _, _ = o.activities.ActivityFacets(ctx, userID, port.ActivityListFilter{})
	out.Recent, _ = o.activities.ListRecentActivitiesForUser(ctx, userID, RecentCount)

	thisStart, lastStart := WeekBounds(o.now())
	out.ThisWeekFrom, out.ThisWeekTo = thisStart, thisStart.AddDate(0, 0, 7)
	out.LastWeekFrom, out.LastWeekTo = lastStart, thisStart
	out.ThisWeek, _ = o.activities.ActivityTotalsForRange(ctx, userID, out.ThisWeekFrom, out.ThisWeekTo)
	out.LastWeek, _ = o.activities.ActivityTotalsForRange(ctx, userID, out.LastWeekFrom, out.LastWeekTo)
	return out, nil
}
