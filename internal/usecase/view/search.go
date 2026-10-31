package view

import (
	"context"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// SearchResult is one page of the faceted activity list.
type SearchResult struct {
	Activities  []domain.Activity
	Types       []port.ActivityFacet
	Disciplines []port.ActivityFacet
	// Years are deliberately unfiltered: the year selector always offers
	// every year with data.
	Years []port.ActivityYearStat
	// Total counts every activity of the user, Matched those matching the
	// filter (ignoring paging).
	Total, Matched int
	// GearPrompt: the user owns gear, so "gear not set" is worth prompting.
	GearPrompt bool
}

// GearLister is the slice of the gear service the search needs.
type GearLister interface {
	List(ctx context.Context, userID domain.UserID, includeRetired bool) ([]domain.Gear, error)
}

// Searches runs the faceted activity list (GET /api/activities/feed,
// ActivityService.SearchActivities).
type Searches struct {
	activities port.ActivityReader
	gear       GearLister // optional
}

// NewSearches wires the repos; gear may be nil.
func NewSearches(activities port.ActivityReader, gear GearLister) *Searches {
	return &Searches{activities: activities, gear: gear}
}

// Paging bounds of the list.
const (
	SearchDefaultLimit = 50
	SearchMaxLimit     = 200
)

// Search applies filter (Limit/Offset included) for userID.
func (s *Searches) Search(ctx context.Context, userID domain.UserID, filter port.ActivityListFilter) (SearchResult, error) {
	if filter.Limit <= 0 {
		filter.Limit = SearchDefaultLimit
	}
	if filter.Limit > SearchMaxLimit {
		filter.Limit = SearchMaxLimit
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	var out SearchResult
	var err error
	out.Types, out.Disciplines, out.Total, err = s.activities.ActivityFacets(ctx, userID, filter)
	if err != nil {
		return out, fmt.Errorf("activity facets: %w", err)
	}
	out.Activities, out.Matched, err = s.activities.ListActivitiesFiltered(ctx, userID, filter)
	if err != nil {
		return out, fmt.Errorf("filtered activities: %w", err)
	}
	out.Years, _ = s.activities.ActivityYearStats(ctx, userID)
	if s.gear != nil {
		if gear, gerr := s.gear.List(ctx, userID, false); gerr == nil && len(gear) > 0 {
			out.GearPrompt = true
		}
	}
	return out, nil
}

// GearUnset reports whether a should prompt for gear: none attached, the
// user never said "no gear", and prompting makes sense (they own gear).
func GearUnset(a domain.Activity, prompt bool) bool {
	return prompt && a.GearID == nil && a.GearSetBy != domain.GearSetByUser
}
