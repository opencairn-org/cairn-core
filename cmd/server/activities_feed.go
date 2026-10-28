package main

import (
	"github.com/google/uuid"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// activities_feed exposes a faceted, filterable, sortable activity feed as a
// plain JSON REST endpoint (the SPA fetches it client-side so filter/sort
// changes don't need a full page reload). It complements the Connect
// ActivityService, which serves typed single-activity reads.
//
//	GET /api/activities/feed
//	  ?type=&discipline=&sort=&limit=&offset=
//	  &from=&to=                                   date range, date-only or RFC3339, half-open
//	  &virtual=&ebike=&commute=&race=              tri-state flags: ""=any, true, false
//	  &distance_min_m=&distance_max_m=             numeric ranges, inclusive, SI units
//	  &duration_min_s=&duration_max_s=
//	  &elevation_min_m=&elevation_max_m=
//	  &speed_min_mps=&speed_max_mps=               average speed
//	  &hr_min_bpm=&hr_max_bpm=                     average heart rate
//	  &power_min_w=&power_max_w=                   average power
//	  → { total, matched, facets:{types:[{value,count}],disciplines:[...],years:[...]}, activities:[...] }
//
// facets list ONLY the values the user actually has, so the filter UI can avoid
// offering impossible options; each facet respects every filter except its own
// dimension. `matched` is the count for the active filter (ignoring limit),
// for an accurate summary; `total` is the grand total.

type feedActivity struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Type             string   `json:"type"`
	Discipline       string   `json:"discipline"`
	StartTime        string   `json:"start_time"`
	Timezone         string   `json:"timezone"`
	ElapsedDurationS int64    `json:"elapsed_duration_s"`
	DistanceM        *float64 `json:"distance_m"`
	ElevationGainM   *float64 `json:"elevation_gain_m"`
	TSS              *float64 `json:"tss"`
	StartPlace       string   `json:"start_place"`
	// GearUnset: no gear and the user has not decided "no gear" either.
	GearUnset bool `json:"gear_unset"`
}

type feedFacet struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// parseFeedDate accepts a date-only (YYYY-MM-DD) or full RFC3339 timestamp and
// returns it as a UTC time; zero time when empty/invalid (→ unbounded).
func parseFeedDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// feedBool parses a tri-state flag param: "" → nil (any), else true/false.
// Invalid values are treated as "any" rather than erroring.
func feedBool(s string) *bool {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return nil
	}
	return &v
}

// feedFloat parses an optional non-negative numeric bound: "" or invalid → nil.
func feedFloat(s string) *float64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return nil
	}
	return &v
}

func toFeedFacets(in []port.ActivityFacet) []feedFacet {
	out := make([]feedFacet, 0, len(in))
	for _, f := range in {
		out = append(out, feedFacet{Value: f.Value, Count: f.Count})
	}
	return out
}

// parseActivityFilter reads the filter query params documented above into a
// port.ActivityListFilter. Shared by every endpoint that takes the standard
// activity filter (feed, heatmap) so the UI's filter bar means the same thing
// everywhere. Paging is left to the caller.
func parseActivityFilter(q url.Values) port.ActivityListFilter {
	return port.ActivityListFilter{
		Type:       q.Get("type"),
		Discipline: q.Get("discipline"),
		Sort:       q.Get("sort"),
		From:       parseFeedDate(q.Get("from")),
		To:         parseFeedDate(q.Get("to")),

		IsVirtual: feedBool(q.Get("virtual")),
		IsEbike:   feedBool(q.Get("ebike")),
		IsCommute: feedBool(q.Get("commute")),
		IsRace:    feedBool(q.Get("race")),

		DistanceMinM:   feedFloat(q.Get("distance_min_m")),
		DistanceMaxM:   feedFloat(q.Get("distance_max_m")),
		DurationMinS:   feedFloat(q.Get("duration_min_s")),
		DurationMaxS:   feedFloat(q.Get("duration_max_s")),
		ElevationMinM:  feedFloat(q.Get("elevation_min_m")),
		ElevationMaxM:  feedFloat(q.Get("elevation_max_m")),
		AvgSpeedMinMps: feedFloat(q.Get("speed_min_mps")),
		AvgSpeedMaxMps: feedFloat(q.Get("speed_max_mps")),
		AvgHRMinBpm:    feedFloat(q.Get("hr_min_bpm")),
		AvgHRMaxBpm:    feedFloat(q.Get("hr_max_bpm")),
		AvgPowerMinW:   feedFloat(q.Get("power_min_w")),
		AvgPowerMaxW:   feedFloat(q.Get("power_max_w")),
		GearIDs:        feedGearIDs(q["gear_id"]),
	}
}

// feedGearIDsMax bounds a repeated ?gear_id= filter.
const feedGearIDsMax = 50

// feedGearIDs parses repeated ?gear_id= values, deduplicated and capped;
// malformed ids are dropped.
func feedGearIDs(raw []string) []domain.GearID {
	var out []domain.GearID
	seen := map[domain.GearID]struct{}{}
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			continue
		}
		gid := domain.GearID(id)
		if _, dup := seen[gid]; dup {
			continue
		}
		seen[gid] = struct{}{}
		out = append(out, gid)
		if len(out) == feedGearIDsMax {
			break
		}
	}
	return out
}

func mountActivitiesFeed(mux *http.ServeMux, app *App, logger *slog.Logger) {
	mux.HandleFunc("GET /api/activities/feed", activitiesFeedHandler(app, logger))

	logger.Info("activities feed endpoint mounted")
}

// activitiesFeedPaging reads limit/offset: default 50, capped at 200, so the feed pages instead of
// returning the (potentially huge) full set.
func activitiesFeedPaging(q url.Values) (limit, offset int) {
	limit, _ = strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset, _ = strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func activitiesFeedHandler(app *App, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		limit, offset := activitiesFeedPaging(q)
		filter := parseActivityFilter(q)
		filter.Limit = limit
		filter.Offset = offset

		res, err := app.Searches.Search(r.Context(), userID, filter)
		if err != nil {
			logger.Error("activity search failed", "error", err)
			http.Error(w, "list failed", http.StatusInternalServerError)
			return
		}
		years := make([]feedFacet, 0, len(res.Years))
		for _, y := range res.Years {
			years = append(years, feedFacet{Value: strconv.Itoa(y.Year), Count: y.Count})
		}
		out := make([]feedActivity, 0, len(res.Activities))
		for _, a := range res.Activities {
			out = append(out, feedActivityOf(a, res.GearPrompt))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"total":   res.Total,
			"matched": res.Matched,
			"facets": map[string]any{
				"types":       toFeedFacets(res.Types),
				"disciplines": toFeedFacets(res.Disciplines),
				"years":       years,
			},
			"activities": out,
			"offset":     offset,
			"limit":      limit,
			"has_more":   offset+len(out) < res.Matched,
		})
	}
}

// feedActivityOf projects an activity into the feed's row shape.
func feedActivityOf(a domain.Activity, gearPrompt bool) feedActivity {
	return feedActivity{
		ID:               a.ID.String(),
		Title:            a.Title,
		Type:             string(a.Type),
		Discipline:       string(a.Discipline),
		StartTime:        a.StartTime.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Timezone:         a.Timezone,
		ElapsedDurationS: int64(a.ElapsedDuration.Seconds()),
		DistanceM:        a.Summary.DistanceM,
		ElevationGainM:   a.Summary.ElevationGainM,
		TSS:              a.Summary.TSS,
		StartPlace:       a.StartPlace,
		GearUnset:        view.GearUnset(a, gearPrompt),
	}
}
