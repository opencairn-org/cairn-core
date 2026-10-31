package rest

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	"github.com/opencairn-org/cairn-core/internal/usecase/segment"
	"github.com/opencairn-org/cairn-core/internal/usecase/segmentview"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// SegmentHandlers serve the segment library, authoring, browsing and
// leaderboards (the REST twin of SegmentService).
//
// MountMine:
//
//	GET    /api/segments                         ?type=&limit=&offset=
//	GET    /api/segments/{id}
//	GET    /api/activities/{id}/segment-efforts
//
// Mount:
//
//	GET    /api/segments/browse        ?sources=&scopes=&types=&with_efforts=&starred=&q=&sort=&limit=&offset=
//	GET    /api/segments/bbox          ?min_lat=&min_lon=&max_lat=&max_lon=&types=&include_instance=&limit=
//	GET    /api/segments/{id}/efforts  ?user_id=&from=&to=&sort=fastest|newest&limit=&offset=
//	GET    /api/segments/{id}/leaderboard ?from=&to=&limit=&offset=
//	POST   /api/segments               { name, description, activity_type, polyline, precision, scope, ..., backfill }
//	PUT    /api/segments/{id}          { name?, description?, scope?, ..., starred?, polyline? }
//	DELETE /api/segments/{id}
//	POST   /api/segments/{id}/rematch  { from?, to? }
//	POST   /api/activities/{id}/rematch-segments
type SegmentHandlers struct {
	Auth  *Auth
	Views *segmentview.Segments
	// Authoring enables the write routes; nil leaves them unmounted.
	Authoring  *segment.Authoring
	Details    *view.Details
	Activities *activity.SourceActions
	// Backfill starts a background rematch of the segment and returns its job id.
	Backfill func(userID domain.UserID, id domain.SegmentID, from, to time.Time) string
	Log      *slog.Logger
}

// MountMine registers the caller's segment library and an activity's efforts.
func (h *SegmentHandlers) MountMine(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/segments", h.list)
	mux.HandleFunc("GET /api/segments/{id}", h.detail)
	mux.HandleFunc("GET /api/activities/{id}/segment-efforts", h.activityEfforts)
}

// Mount registers browsing, leaderboards and, with Authoring, the writes.
func (h *SegmentHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/segments/browse", h.browse)
	mux.HandleFunc("GET /api/segments/bbox", h.bbox)
	mux.HandleFunc("GET /api/segments/{id}/efforts", h.efforts)
	mux.HandleFunc("GET /api/segments/{id}/leaderboard", h.leaderboard)
	if h.Authoring != nil {
		mux.HandleFunc("POST /api/segments", h.create)
		mux.HandleFunc("PUT /api/segments/{id}", h.update)
		mux.HandleFunc("DELETE /api/segments/{id}", h.del)
		mux.HandleFunc("POST /api/segments/{id}/rematch", h.rematchSegment)
		mux.HandleFunc("POST /api/activities/{id}/rematch-segments", h.rematchActivity)
	}
}

func (h *SegmentHandlers) segmentID(w http.ResponseWriter, r *http.Request) (domain.SegmentID, bool) {
	id, err := domain.ParseUUID[domain.SegmentID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return id, false
	}
	return id, true
}

func (h *SegmentHandlers) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	limit, offset := segListPaging(r)
	page, err := h.Views.Mine(r.Context(), userID, r.URL.Query().Get("type"), limit, offset)
	if err != nil {
		h.Log.Error("list segments failed", "user", userID, "error", err)
		writeActionError(w, err, "load failed")
		return
	}
	var stats domain.UserSegmentStats
	if page.Stats != nil {
		stats = *page.Stats
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stats": map[string]any{
			"segments": stats.Segments,
			"efforts":  stats.Efforts,
			"prs":      stats.PRs,
			"crs":      stats.CRs,
			"external": stats.External,
			"native":   stats.Native,
		},
		"segments": segListItemsJSON(page.Items),
		"limit":    limit,
		"offset":   offset,
		"has_more": page.HasMore,
	})
}

func (h *SegmentHandlers) detail(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := h.segmentID(w, r)
	if !ok {
		return
	}
	seg, efforts, err := h.Views.Detail(r.Context(), userID, id)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"segment": map[string]any{
			"id":                 seg.ID.String(),
			"name":               seg.Name,
			"distance_m":         seg.DistanceM,
			"activity_type":      string(seg.ActivityType),
			"climb_category":     segClimbJSON(seg),
			"polyline":           seg.Polyline,
			"polyline_precision": seg.PolylinePrecision,
			"elevation_gain_m":   seg.ElevationGainM,
			"avg_grade":          seg.AvgGrade,
			"max_grade":          seg.MaxGrade,
		},
		"efforts": segEffortsJSON(efforts),
	})
}

func (h *SegmentHandlers) activityEfforts(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad activity id", http.StatusBadRequest)
		return
	}
	act, err := h.Activities.OwnedActivity(r.Context(), userID, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rows, err := h.Details.SegmentEfforts(r.Context(), act)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, segEffortRowJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"efforts": out})
}

func (h *SegmentHandlers) browse(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := domain.SegmentListFilter{
		Sources: segCSV[domain.SegmentSource](q.Get("sources")), Scopes: segCSV[domain.SegmentScope](q.Get("scopes")),
		ActivityTypes: segCSV[domain.ActivityType](q.Get("types")), OnlyWithEfforts: q.Get("with_efforts") == "true",
		OnlyStarred: q.Get("starred") == "true", Query: q.Get("q"), Sort: domain.SegmentSort(q.Get("sort")),
		Limit: parseIntQuery(r, "limit", segmentview.SegmentsDefaultLimit), Offset: parseIntQuery(r, "offset", 0),
	}
	segs, total, err := h.Views.List(r.Context(), userID, f)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"segments": segListJSON(segs), "total": total, "limit": f.Limit, "offset": f.Offset})
}

func (h *SegmentHandlers) bbox(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := func(k string) (float64, bool) {
		v, err := strconv.ParseFloat(q.Get(k), 64)
		return v, err == nil
	}
	minLat, ok1 := f("min_lat")
	minLon, ok2 := f("min_lon")
	maxLat, ok3 := f("max_lat")
	maxLon, ok4 := f("max_lon")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		http.Error(w, "min_lat, min_lon, max_lat and max_lon are required", http.StatusBadRequest)
		return
	}
	segs, err := h.Views.SearchBBox(r.Context(), userID, port.BoundingBox{MinLat: minLat, MinLon: minLon, MaxLat: maxLat, MaxLon: maxLon},
		segCSV[domain.ActivityType](q.Get("types")), q.Get("include_instance") == "true", parseIntQuery(r, "limit", 100))
	if err != nil {
		writeActionError(w, err, "search failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"segments": segListJSON(segs)})
}

func (h *SegmentHandlers) efforts(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := h.segmentID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	eq := domain.SegmentEffortQuery{FastestFirst: q.Get("sort") == "fastest", Limit: parseIntQuery(r, "limit", segmentview.SegmentsDefaultLimit), Offset: parseIntQuery(r, "offset", 0)}
	eq.From, eq.To = segTime(q.Get("from")), segTime(q.Get("to"))
	if raw := q.Get("user_id"); raw != "" {
		u, err := domain.ParseUUID[domain.UserID](raw)
		if err != nil {
			http.Error(w, "bad user_id", http.StatusBadRequest)
			return
		}
		eq.UserID = &u
	}
	efforts, err := h.Views.Efforts(r.Context(), userID, id, eq)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"efforts": segEffortsJSON(efforts)})
}

func (h *SegmentHandlers) leaderboard(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := h.segmentID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	lq := domain.LeaderboardQuery{Limit: parseIntQuery(r, "limit", segmentview.SegmentsDefaultLimit), Offset: parseIntQuery(r, "offset", 0)}
	lq.From, lq.To = segTime(q.Get("from")), segTime(q.Get("to"))
	entries, mine, err := h.Views.Leaderboard(r.Context(), userID, id, lq)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := map[string]any{"entries": segLeaderboardJSON(entries)}
	if mine != nil {
		out["my_rank"] = mine.Rank
		out["my_entry"] = segLeaderboardJSON([]domain.LeaderboardEntry{*mine})[0]
	}
	writeJSON(w, http.StatusOK, out)
}

type segBody struct {
	Name                 *string  `json:"name"`
	Description          *string  `json:"description"`
	ActivityType         string   `json:"activity_type"`
	Polyline             *string  `json:"polyline"`
	Precision            int      `json:"precision"`
	Scope                *string  `json:"scope"`
	MatchCorridorM       *float64 `json:"match_corridor_m"`
	MatchStartToleranceM *float64 `json:"match_start_tolerance_m"`
	MatchEndToleranceM   *float64 `json:"match_end_tolerance_m"`
	Bidirectional        *bool    `json:"bidirectional"`
	Starred              *bool    `json:"starred"`
	Backfill             *bool    `json:"backfill"`
	KeepEfforts          bool     `json:"keep_efforts"`
	From                 string   `json:"from"`
	To                   string   `json:"to"`
}

func (h *SegmentHandlers) create(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok || !requireJSON(w, r) {
		return
	}
	var b segBody
	if err := decodeJSONBody(r, &b); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	seg, err := h.Authoring.Create(r.Context(), userID, domain.NativeSegmentInput{
		Name: segDeref(b.Name), Description: segDeref(b.Description), ActivityType: domain.ActivityType(b.ActivityType),
		Polyline: segDeref(b.Polyline), PolylinePrecision: b.Precision, Scope: domain.SegmentScope(segDeref(b.Scope)),
		MatchCorridorM: b.MatchCorridorM, MatchStartToleranceM: b.MatchStartToleranceM, MatchEndToleranceM: b.MatchEndToleranceM,
		Bidirectional: segDeref(b.Bidirectional),
	})
	if err != nil {
		writeActionError(w, err, "create failed")
		return
	}
	out := map[string]any{"segment": segJSON(seg)}
	if b.Backfill == nil || *b.Backfill {
		out["backfill_job_id"] = h.Backfill(userID, seg.ID, time.Time{}, time.Time{})
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *SegmentHandlers) update(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok || !requireJSON(w, r) {
		return
	}
	id, ok := h.segmentID(w, r)
	if !ok {
		return
	}
	var b segBody
	if err := decodeJSONBody(r, &b); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	p := domain.SegmentPatch{
		Name: b.Name, Description: b.Description, MatchCorridorM: b.MatchCorridorM, MatchStartToleranceM: b.MatchStartToleranceM,
		MatchEndToleranceM: b.MatchEndToleranceM, Bidirectional: b.Bidirectional, Starred: b.Starred, Polyline: b.Polyline, PolylinePrecision: b.Precision,
	}
	if b.Scope != nil {
		sc := domain.SegmentScope(*b.Scope)
		p.Scope = &sc
	}
	seg, err := h.Authoring.Update(r.Context(), userID, id, p)
	if err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"segment": segJSON(seg)})
}

func (h *SegmentHandlers) del(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := h.segmentID(w, r)
	if !ok {
		return
	}
	if err := h.Authoring.Delete(r.Context(), userID, id, r.URL.Query().Get("keep_efforts") == "true"); err != nil {
		writeActionError(w, err, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *SegmentHandlers) rematchSegment(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := h.segmentID(w, r)
	if !ok {
		return
	}
	var b segBody
	_ = decodeJSONBody(r, &b)
	if _, err := h.Views.Visible(r.Context(), userID, id); err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	job := h.Backfill(userID, id, segTime(b.From), segTime(b.To))
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job})
}

func (h *SegmentHandlers) rematchActivity(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	aid, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	sources, efforts, err := h.Authoring.RematchActivity(r.Context(), userID, aid)
	if err != nil {
		writeActionError(w, err, "rematch failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources, "efforts": efforts})
}

// segListPaging parses limit (default 50, max 200) and offset.
func segListPaging(r *http.Request) (limit, offset int) {
	limit = 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 200)
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	return limit, offset
}

// segCSV splits a comma-separated query value, dropping blanks.
func segCSV[T ~string](raw string) []T {
	var out []T
	for p := range strings.SplitSeq(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, T(p))
		}
	}
	return out
}

// segTime parses an RFC 3339 query value; empty or invalid is the zero time.
func segTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func segDeref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// segClimbJSON returns the climb category, or nil when uncategorised.
func segClimbJSON(seg domain.Segment) any {
	if seg.ClimbCategory != "" && seg.ClimbCategory != domain.ClimbCategoryNone {
		return string(seg.ClimbCategory)
	}
	return nil
}

func segListItemsJSON(items []domain.UserSegmentListItem) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"id":               it.ID.String(),
			"name":             it.Name,
			"activity_type":    it.ActivityType,
			"source":           string(it.Source),
			"distance_m":       it.DistanceM,
			"elevation_gain_m": it.ElevationGainM,
			"avg_grade":        it.AvgGrade,
			"effort_count":     it.EffortCount,
			"best_elapsed_s":   it.BestElapsedS,
			"last_effort_at":   it.LastEffortAt.UTC(),
			"has_pr":           it.HasPR,
			"has_cr":           it.HasCR,
		})
	}
	return out
}

func segEffortsJSON(efforts []domain.SegmentEffort) []map[string]any {
	out := make([]map[string]any, 0, len(efforts))
	for _, e := range efforts {
		out = append(out, map[string]any{
			"id":                 e.ID.String(),
			"activity_id":        e.ActivityID.String(),
			"start_time":         e.StartTime.UTC().Format(time.RFC3339),
			"elapsed_s":          int64(e.ElapsedS),
			"moving_s":           int64(e.MovingS),
			"avg_heart_rate":     e.AvgHeartRateBpm,
			"avg_power":          e.AvgPowerW,
			"personal_rank":      e.PersonalRank,
			"is_personal_record": e.IsPersonalRecord,
		})
	}
	return out
}

// segEffortRowJSON renders one effort, located on the stream when possible.
func segEffortRowJSON(row view.EffortRow) map[string]any {
	e, seg := row.Effort, row.Segment
	out := map[string]any{
		"segment_id":         e.SegmentID.String(),
		"segment_name":       seg.Name,
		"segment_distance_m": seg.DistanceM,
		"climb_category":     segClimbJSON(seg),
		"elapsed_s":          e.ElapsedS,
		"moving_s":           e.MovingS,
		"avg_heart_rate":     e.AvgHeartRateBpm,
		"avg_power":          e.AvgPowerW,
		"is_personal_record": e.IsPersonalRecord,
		"personal_rank":      e.PersonalRank,
	}
	if row.StartOffsetS != nil {
		out["start_offset_s"] = *row.StartOffsetS
		out["end_offset_s"] = *row.EndOffsetS
	}
	if row.StartDistanceM != nil {
		out["start_distance_m"] = *row.StartDistanceM
	}
	if row.EndDistanceM != nil {
		out["end_distance_m"] = *row.EndDistanceM
	}
	return out
}

func segListJSON(segs []domain.Segment) []map[string]any {
	out := make([]map[string]any, 0, len(segs))
	for _, s := range segs {
		out = append(out, segJSON(s))
	}
	return out
}

func segJSON(seg domain.Segment) map[string]any {
	m := map[string]any{
		"id": seg.ID.String(), "source": string(seg.Source), "scope": string(seg.Scope), "name": seg.Name,
		"description": seg.Description, "activity_type": string(seg.ActivityType), "polyline": seg.Polyline,
		"polyline_precision": seg.PolylinePrecision, "distance_m": seg.DistanceM, "elevation_gain_m": seg.ElevationGainM,
		"avg_grade": seg.AvgGrade, "max_grade": seg.MaxGrade, "climb_category": segClimbJSON(seg),
		"match_corridor_m": seg.MatchCorridorM, "match_start_tolerance_m": seg.MatchStartToleranceM,
		"match_end_tolerance_m": seg.MatchEndToleranceM, "bidirectional": seg.Bidirectional, "starred": seg.Starred,
		"created_at": seg.CreatedAt.UTC().Format(time.RFC3339), "updated_at": seg.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if seg.OwnerUserID != nil {
		m["owner_user_id"] = seg.OwnerUserID.String()
	}
	return m
}

func segLeaderboardJSON(entries []domain.LeaderboardEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"rank": e.Rank, "user_id": e.UserID.String(), "display_name": e.DisplayName, "avatar_url": e.AvatarURL,
			"elapsed_s": e.ElapsedS, "start_time": e.StartTime.UTC().Format(time.RFC3339),
			"effort_id": e.EffortID.String(), "activity_id": e.ActivityID.String(),
		})
	}
	return out
}
