package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// mountSegmentAuthoring is the REST twin of the SegmentService authoring,
// browsing and leaderboard RPCs:
//
//	GET    /api/segments/browse        ?sources=&scopes=&types=&with_efforts=&starred=&q=&sort=&limit=&offset=
//	GET    /api/segments/bbox          ?min_lat=&min_lon=&max_lat=&max_lon=&types=&include_instance=&limit=
//	POST   /api/segments               { name, description, activity_type, polyline, precision, scope, ..., backfill }
//	PUT    /api/segments/{id}          { name?, description?, scope?, ..., starred?, polyline? }
//	DELETE /api/segments/{id}
//	GET    /api/segments/{id}/efforts  ?user_id=&from=&to=&sort=fastest|newest&limit=&offset=
//	GET    /api/segments/{id}/leaderboard ?from=&to=&limit=&offset=
//	POST   /api/segments/{id}/rematch  { from?, to? }
//	POST   /api/activities/{id}/rematch-segments
func mountSegmentAuthoring(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.SegmentViews == nil {
		return
	}
	h := &segmentAuthorHandlers{app: app, logger: logger}
	mux.HandleFunc("GET /api/segments/browse", h.browse)
	mux.HandleFunc("GET /api/segments/bbox", h.bbox)
	mux.HandleFunc("GET /api/segments/{id}/efforts", h.efforts)
	mux.HandleFunc("GET /api/segments/{id}/leaderboard", h.leaderboard)
	if app.SegmentAuthoring != nil {
		mux.HandleFunc("POST /api/segments", h.create)
		mux.HandleFunc("PUT /api/segments/{id}", h.update)
		mux.HandleFunc("DELETE /api/segments/{id}", h.del)
		mux.HandleFunc("POST /api/segments/{id}/rematch", h.rematchSegment)
		mux.HandleFunc("POST /api/activities/{id}/rematch-segments", h.rematchActivity)
	}
}

type segmentAuthorHandlers struct {
	app    *App
	logger *slog.Logger
}

func (h *segmentAuthorHandlers) caller(w http.ResponseWriter, r *http.Request) (domain.UserID, bool) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
	}
	return userID, ok
}

func (h *segmentAuthorHandlers) segmentID(w http.ResponseWriter, r *http.Request) (domain.SegmentID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return domain.SegmentID{}, false
	}
	return domain.SegmentID(id), true
}

func csv[T ~string](raw string) []T {
	var out []T
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, T(p))
		}
	}
	return out
}

func (h *segmentAuthorHandlers) browse(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := domain.SegmentListFilter{
		Sources: csv[domain.SegmentSource](q.Get("sources")), Scopes: csv[domain.SegmentScope](q.Get("scopes")),
		ActivityTypes: csv[domain.ActivityType](q.Get("types")), OnlyWithEfforts: q.Get("with_efforts") == "true",
		OnlyStarred: q.Get("starred") == "true", Query: q.Get("q"), Sort: domain.SegmentSort(q.Get("sort")),
		Limit: parseIntQuery(r, "limit", view.SegmentsDefaultLimit), Offset: parseIntQuery(r, "offset", 0),
	}
	segs, total, err := h.app.SegmentViews.List(r.Context(), userID, f)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"segments": segmentsJSON(segs), "total": total, "limit": f.Limit, "offset": f.Offset})
}

func (h *segmentAuthorHandlers) bbox(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
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
	segs, err := h.app.SegmentViews.SearchBBox(r.Context(), userID, port.BoundingBox{MinLat: minLat, MinLon: minLon, MaxLat: maxLat, MaxLon: maxLon},
		csv[domain.ActivityType](q.Get("types")), q.Get("include_instance") == "true", parseIntQuery(r, "limit", 100))
	if err != nil {
		writeActionError(w, err, "search failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"segments": segmentsJSON(segs)})
}

func (h *segmentAuthorHandlers) efforts(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
	if !ok {
		return
	}
	id, ok := h.segmentID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	eq := domain.SegmentEffortQuery{FastestFirst: q.Get("sort") == "fastest", Limit: parseIntQuery(r, "limit", view.SegmentsDefaultLimit), Offset: parseIntQuery(r, "offset", 0)}
	eq.From, eq.To = parseTimeQuery(q.Get("from")), parseTimeQuery(q.Get("to"))
	if raw := q.Get("user_id"); raw != "" {
		uid, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "bad user_id", http.StatusBadRequest)
			return
		}
		u := domain.UserID(uid)
		eq.UserID = &u
	}
	efforts, err := h.app.SegmentViews.Efforts(r.Context(), userID, id, eq)
	if err != nil {
		writeSegmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"efforts": segmentDetailEffortsJSON(efforts)})
}

func (h *segmentAuthorHandlers) leaderboard(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
	if !ok {
		return
	}
	id, ok := h.segmentID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	lq := domain.LeaderboardQuery{Limit: parseIntQuery(r, "limit", view.SegmentsDefaultLimit), Offset: parseIntQuery(r, "offset", 0)}
	lq.From, lq.To = parseTimeQuery(q.Get("from")), parseTimeQuery(q.Get("to"))
	entries, mine, err := h.app.SegmentViews.Leaderboard(r.Context(), userID, id, lq)
	if err != nil {
		writeSegmentError(w, err)
		return
	}
	out := map[string]any{"entries": leaderboardJSON(entries)}
	if mine != nil {
		out["my_rank"] = mine.Rank
		out["my_entry"] = leaderboardJSON([]domain.LeaderboardEntry{*mine})[0]
	}
	writeJSON(w, http.StatusOK, out)
}

type segmentBody struct {
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

func (h *segmentAuthorHandlers) create(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
	if !ok || !requireJSON(w, r) {
		return
	}
	var b segmentBody
	if err := decodeJSONBody(r, &b); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	seg, err := h.app.SegmentAuthoring.Create(r.Context(), userID, domain.NativeSegmentInput{
		Name: deref(b.Name), Description: deref(b.Description), ActivityType: domain.ActivityType(b.ActivityType),
		Polyline: deref(b.Polyline), PolylinePrecision: b.Precision, Scope: domain.SegmentScope(deref(b.Scope)),
		MatchCorridorM: b.MatchCorridorM, MatchStartToleranceM: b.MatchStartToleranceM, MatchEndToleranceM: b.MatchEndToleranceM,
		Bidirectional: deref(b.Bidirectional),
	})
	if err != nil {
		writeActionError(w, err, "create failed")
		return
	}
	out := map[string]any{"segment": segmentJSON(seg)}
	if b.Backfill == nil || *b.Backfill {
		out["backfill_job_id"] = backfillSegment(h.app, h.logger, userID, seg.ID, time.Time{}, time.Time{})
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *segmentAuthorHandlers) update(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
	if !ok || !requireJSON(w, r) {
		return
	}
	id, ok := h.segmentID(w, r)
	if !ok {
		return
	}
	var b segmentBody
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
	seg, err := h.app.SegmentAuthoring.Update(r.Context(), userID, id, p)
	if err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"segment": segmentJSON(seg)})
}

func (h *segmentAuthorHandlers) del(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
	if !ok {
		return
	}
	id, ok := h.segmentID(w, r)
	if !ok {
		return
	}
	if err := h.app.SegmentAuthoring.Delete(r.Context(), userID, id, r.URL.Query().Get("keep_efforts") == "true"); err != nil {
		writeActionError(w, err, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *segmentAuthorHandlers) rematchSegment(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
	if !ok {
		return
	}
	id, ok := h.segmentID(w, r)
	if !ok {
		return
	}
	var b segmentBody
	_ = decodeJSONBody(r, &b)
	if _, err := h.app.SegmentViews.Visible(r.Context(), userID, id); err != nil {
		writeSegmentError(w, err)
		return
	}
	job := backfillSegment(h.app, h.logger, userID, id, parseTimeQuery(b.From), parseTimeQuery(b.To))
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job})
}

func (h *segmentAuthorHandlers) rematchActivity(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
	if !ok {
		return
	}
	aid, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	sources, efforts, err := h.app.SegmentAuthoring.RematchActivity(r.Context(), userID, domain.ActivityID(aid))
	if err != nil {
		writeActionError(w, err, "rematch failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources, "efforts": efforts})
}

// backfillSegment runs the segment rematch in the background and returns its job id.
func backfillSegment(app *App, logger *slog.Logger, userID domain.UserID, id domain.SegmentID, from, to time.Time) string {
	job := "rematch-segment:" + id.String()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		sources, failed, err := app.SegmentAuthoring.RematchSegment(ctx, userID, id, from, to)
		logger.Info("segment rematch finished", "job_id", job, "sources", sources, "failed", failed, "error", err)
	}()
	return job
}

// writeSegmentError maps the view's not-found sentinel, else the action error.
func writeSegmentError(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeActionError(w, err, "load failed")
}

func parseTimeQuery(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func segmentsJSON(segs []domain.Segment) []map[string]any {
	out := make([]map[string]any, 0, len(segs))
	for _, s := range segs {
		out = append(out, segmentJSON(s))
	}
	return out
}

func segmentJSON(seg domain.Segment) map[string]any {
	m := map[string]any{
		"id": seg.ID.String(), "source": string(seg.Source), "scope": string(seg.Scope), "name": seg.Name,
		"description": seg.Description, "activity_type": string(seg.ActivityType), "polyline": seg.Polyline,
		"polyline_precision": seg.PolylinePrecision, "distance_m": seg.DistanceM, "elevation_gain_m": seg.ElevationGainM,
		"avg_grade": seg.AvgGrade, "max_grade": seg.MaxGrade, "climb_category": segmentClimbJSON(seg),
		"match_corridor_m": seg.MatchCorridorM, "match_start_tolerance_m": seg.MatchStartToleranceM,
		"match_end_tolerance_m": seg.MatchEndToleranceM, "bidirectional": seg.Bidirectional, "starred": seg.Starred,
		"created_at": seg.CreatedAt.UTC().Format(time.RFC3339), "updated_at": seg.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if seg.OwnerUserID != nil {
		m["owner_user_id"] = seg.OwnerUserID.String()
	}
	return m
}

func leaderboardJSON(entries []domain.LeaderboardEntry) []map[string]any {
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
