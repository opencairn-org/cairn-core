package rest

import (
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Renderers for the multi-user read path: every non-owner read resolves the
// viewer through view.Visibility and renders the resulting domain projection,
// so gated data never reaches the JSON.

// projectActivityJSON emits an activity as JSON limited to the allowed
// categories, rendered from the domain projection the Connect
// ProjectedActivity message is also built from.
func projectActivityJSON(a domain.Activity, cats domain.CategorySet, zones []domain.PrivacyZone) map[string]any {
	return projectedJSON(domain.ProjectActivity(a, cats, zones))
}

// projectedJSON renders a domain projection in the REST shape. Map and photo
// URLs point at the gated REST routes (the web UI's contract).
func projectedJSON(p domain.ProjectedActivity) map[string]any {
	out := map[string]any{
		"id":                 p.ID.String(),
		"title":              p.Title,
		"type":               string(p.Type),
		"discipline":         string(p.Discipline),
		"start_time":         p.StartTime.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"timezone":           p.Timezone,
		"elapsed_duration_s": int64(p.ElapsedDuration.Seconds()),
		"moving_duration_s":  int64(p.MovingDuration.Seconds()),
		"distance_m":         p.DistanceM,
		"elevation_gain_m":   p.ElevationGainM,
	}
	has := projectedHas(p)
	if has[domain.CategorySummary] {
		out["description"] = p.Description
		out["tss"] = p.TSS
		out["calories"] = p.CaloriesKcal
	}
	if has[domain.CategoryLocation] {
		projectedLocationJSON(out, p)
	}
	if has[domain.CategoryHR] {
		out["avg_hr"] = p.AvgHeartRateBpm
		out["max_hr"] = p.MaxHeartRateBpm
	}
	if has[domain.CategoryPower] {
		out["avg_power"] = p.AvgPowerW
		out["max_power"] = p.MaxPowerW
		out["normalized_power"] = p.NormalizedPowerW
	}
	if has[domain.CategoryPaceSpeed] {
		out["avg_speed_mps"] = p.AvgSpeedMps
		out["max_speed_mps"] = p.MaxSpeedMps
	}
	if p.MapVisible {
		out["map_image_url"] = "/api/activities/" + p.ID.String() + "/map.png"
	}
	if p.PhotosVisible {
		out["photos_url"] = "/api/activities/" + p.ID.String() + "/attachments"
	}
	allowed := make([]string, 0, len(p.Visible))
	for _, c := range p.Visible {
		allowed = append(allowed, string(c))
	}
	out["visible_categories"] = allowed
	return out
}

// projectedHas indexes the granted categories.
func projectedHas(p domain.ProjectedActivity) map[domain.Category]bool {
	has := make(map[domain.Category]bool, len(p.Visible))
	for _, c := range p.Visible {
		has[c] = true
	}
	return has
}

// projectedLocationJSON adds the start place/coords unless geofenced.
func projectedLocationJSON(out map[string]any, p domain.ProjectedActivity) {
	if p.StartLocationRedacted {
		out["start_location_redacted"] = true
		return
	}
	out["start_place"] = p.StartPlace
	if p.StartLat != nil && p.StartLng != nil {
		out["start_lat"] = *p.StartLat
		out["start_lng"] = *p.StartLng
	}
}

func userCardsJSON(users []domain.User) []map[string]any {
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, userCardJSON(u))
	}
	return out
}

func userCardJSON(u domain.User) map[string]any {
	return map[string]any{
		"id":           u.ID.String(),
		"username":     u.Username,
		"display_name": u.DisplayName,
		"avatar_url":   u.AvatarURL,
	}
}
