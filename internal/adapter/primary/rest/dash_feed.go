package rest

import (
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// The standard activity filter, shared by the feed and the heatmap so the
// UI's filter bar means the same thing everywhere:
//
//	?type=&discipline=&sort=
//	&from=&to=                                   date range, date-only or RFC3339, half-open
//	&virtual=&ebike=&commute=&race=              tri-state flags: ""=any, true, false
//	&distance_min_m=&distance_max_m=             numeric ranges, inclusive, SI units
//	&duration_min_s=&duration_max_s=
//	&elevation_min_m=&elevation_max_m=
//	&speed_min_mps=&speed_max_mps=               average speed
//	&hr_min_bpm=&hr_max_bpm=                     average heart rate
//	&power_min_w=&power_max_w=                   average power
//	&gear_id=...                                 repeated
//
// Facets list ONLY the values the user actually has; each facet respects every
// filter except its own dimension.

type dashFeedActivity struct {
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

type dashFacet struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// dashParseDate accepts YYYY-MM-DD or RFC3339; zero (unbounded) when empty or invalid.
func dashParseDate(s string) time.Time {
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

// dashBool parses a tri-state flag; "" or invalid → nil (any).
func dashBool(s string) *bool {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return nil
	}
	return &v
}

// dashFloat parses an optional non-negative bound; "" or invalid → nil.
func dashFloat(s string) *float64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return nil
	}
	return &v
}

func dashFacets(in []port.ActivityFacet) []dashFacet {
	out := make([]dashFacet, 0, len(in))
	for _, f := range in {
		out = append(out, dashFacet{Value: f.Value, Count: f.Count})
	}
	return out
}

func dashYearFacets(in []port.ActivityYearStat) []dashFacet {
	out := make([]dashFacet, 0, len(in))
	for _, y := range in {
		out = append(out, dashFacet{Value: strconv.Itoa(y.Year), Count: y.Count})
	}
	return out
}

// dashActivityFilter reads the standard filter; paging is left to the caller.
func dashActivityFilter(q url.Values) port.ActivityListFilter {
	return port.ActivityListFilter{
		Type:       q.Get("type"),
		Discipline: q.Get("discipline"),
		Sort:       q.Get("sort"),
		From:       dashParseDate(q.Get("from")),
		To:         dashParseDate(q.Get("to")),

		IsVirtual: dashBool(q.Get("virtual")),
		IsEbike:   dashBool(q.Get("ebike")),
		IsCommute: dashBool(q.Get("commute")),
		IsRace:    dashBool(q.Get("race")),

		DistanceMinM:   dashFloat(q.Get("distance_min_m")),
		DistanceMaxM:   dashFloat(q.Get("distance_max_m")),
		DurationMinS:   dashFloat(q.Get("duration_min_s")),
		DurationMaxS:   dashFloat(q.Get("duration_max_s")),
		ElevationMinM:  dashFloat(q.Get("elevation_min_m")),
		ElevationMaxM:  dashFloat(q.Get("elevation_max_m")),
		AvgSpeedMinMps: dashFloat(q.Get("speed_min_mps")),
		AvgSpeedMaxMps: dashFloat(q.Get("speed_max_mps")),
		AvgHRMinBpm:    dashFloat(q.Get("hr_min_bpm")),
		AvgHRMaxBpm:    dashFloat(q.Get("hr_max_bpm")),
		AvgPowerMinW:   dashFloat(q.Get("power_min_w")),
		AvgPowerMaxW:   dashFloat(q.Get("power_max_w")),
		GearIDs:        dashGearIDs(q["gear_id"]),
	}
}

// dashGearIDsMax bounds a repeated ?gear_id= filter.
const dashGearIDsMax = 50

// dashGearIDs parses repeated ?gear_id= values, deduplicated and capped;
// malformed ids are dropped.
func dashGearIDs(raw []string) []domain.GearID {
	var out []domain.GearID
	seen := map[domain.GearID]struct{}{}
	for _, s := range raw {
		gid, err := domain.ParseUUID[domain.GearID](s)
		if err != nil {
			continue
		}
		if _, dup := seen[gid]; dup {
			continue
		}
		seen[gid] = struct{}{}
		out = append(out, gid)
		if len(out) == dashGearIDsMax {
			break
		}
	}
	return out
}

// dashFeedPaging reads limit/offset: default 50, capped at 200.
func dashFeedPaging(q url.Values) (limit, offset int) {
	limit, _ = strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = view.SearchDefaultLimit
	}
	if limit > view.SearchMaxLimit {
		limit = view.SearchMaxLimit
	}
	offset, _ = strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// dashFeedActivityOf projects an activity into the feed's row shape.
func dashFeedActivityOf(a domain.Activity, gearPrompt bool) dashFeedActivity {
	return dashFeedActivity{
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

// feed serves the faceted, filterable, sortable activity list.
//
//	GET /api/activities/feed?<filter>&limit=&offset=
//	  → { total, matched, facets:{types,disciplines,years}, activities, offset, limit, has_more }
func (h *DashboardHandlers) feed(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, offset := dashFeedPaging(q)
	filter := dashActivityFilter(q)
	filter.Limit = limit
	filter.Offset = offset

	res, err := h.Searches.Search(r.Context(), userID, filter)
	if err != nil {
		h.Log.Error("activity search failed", "error", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]dashFeedActivity, 0, len(res.Activities))
	for _, a := range res.Activities {
		out = append(out, dashFeedActivityOf(a, res.GearPrompt))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   res.Total,
		"matched": res.Matched,
		"facets": map[string]any{
			"types":       dashFacets(res.Types),
			"disciplines": dashFacets(res.Disciplines),
			"years":       dashYearFacets(res.Years),
		},
		"activities": out,
		"offset":     offset,
		"limit":      limit,
		"has_more":   offset+len(out) < res.Matched,
	})
}

// heatmap serves every GPS track of the owner's matching activities.
//
//	GET /api/activities/heatmap?<filter>&res=30s|5s
//	  → { matched, with_track, truncated, facets:{...},
//	      geojson: FeatureCollection<LineString>{properties:{id,type,discipline}} }
func (h *DashboardHandlers) heatmap(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	res := domain.StreamResolution30s
	if q.Get("res") == string(domain.StreamResolution5s) {
		res = domain.StreamResolution5s
	}
	hm, err := h.Stats.Heatmap(r.Context(), userID, dashActivityFilter(q), res)
	if err != nil {
		h.Log.Error("heatmap failed", "error", err)
		writeActionError(w, err, "tracks failed")
		return
	}
	features := make([]map[string]any, 0, len(hm.Tracks))
	for _, t := range hm.Tracks {
		features = append(features, dashHeatmapFeature(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"matched":    hm.Matched,
		"with_track": len(features),
		"truncated":  hm.Truncated,
		"facets": map[string]any{
			"types":       dashFacets(hm.Types),
			"disciplines": dashFacets(hm.Disciplines),
			"years":       dashYearFacets(hm.Years),
		},
		"geojson": map[string]any{"type": "FeatureCollection", "features": features},
	})
}

// dashHeatmapFeature builds one LineString feature at ~1 m precision.
func dashHeatmapFeature(t view.HeatmapTrack) map[string]any {
	coords := make([][2]float64, len(t.Points))
	for i, p := range t.Points {
		coords[i] = [2]float64{dashRound5(p.Lon), dashRound5(p.Lat)}
	}
	return map[string]any{
		"type": "Feature",
		"id":   t.Activity.ID.String(),
		"properties": map[string]any{
			"id":         t.Activity.ID.String(),
			"type":       string(t.Activity.Type),
			"discipline": string(t.Activity.Discipline),
		},
		"geometry": map[string]any{"type": "LineString", "coordinates": coords},
	}
}

func dashRound5(v float64) float64 { return math.Round(v*1e5) / 1e5 }
