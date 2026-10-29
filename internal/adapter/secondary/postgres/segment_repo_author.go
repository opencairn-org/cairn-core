package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// segmentVisibleSQL is the predicate for "viewer $N may see segment s".
func segmentVisibleSQL(viewerParam string) string {
	return `(
		(s.source = 'native' AND s.scope = 'instance')
		OR (s.source = 'native' AND s.owner_user_id = ` + viewerParam + `)
		OR (s.source = 'external' AND s.external_account_id IN (
			SELECT id FROM external_accounts WHERE user_id = ` + viewerParam + `))
	)`
}

func (r *SegmentRepo) ViewerOwnsAccount(ctx context.Context, viewer domain.UserID, account domain.ExternalAccountID) (bool, error) {
	db := dbtx(ctx, r.pool)
	var ok bool
	err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM external_accounts WHERE id = $1 AND user_id = $2)`,
		account.UUID(), viewer.UUID()).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("viewer owns account: %w", err)
	}
	return ok, nil
}

func (r *SegmentRepo) CountEffortsForSegment(ctx context.Context, segmentID domain.SegmentID) (int, error) {
	db := dbtx(ctx, r.pool)
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM segment_efforts WHERE segment_id = $1`, segmentID.UUID()).Scan(&n); err != nil {
		return 0, fmt.Errorf("count efforts: %w", err)
	}
	return n, nil
}

func (r *SegmentRepo) ListSegments(ctx context.Context, viewer domain.UserID, f domain.SegmentListFilter) ([]domain.Segment, int, error) {
	db := dbtx(ctx, r.pool)
	where := []string{segmentVisibleSQL("$1")}
	args := []any{viewer.UUID()}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if len(f.Sources) > 0 {
		add("s.source = ANY($%d)", stringsOf(f.Sources))
	}
	if len(f.Scopes) > 0 {
		add("s.scope = ANY($%d)", stringsOf(f.Scopes))
	}
	if len(f.ActivityTypes) > 0 {
		add("s.activity_type = ANY($%d)", stringsOf(f.ActivityTypes))
	}
	if f.OnlyStarred {
		where = append(where, "s.starred")
	}
	if f.OnlyWithEfforts {
		where = append(where, "EXISTS (SELECT 1 FROM segment_efforts e WHERE e.segment_id = s.id AND e.user_id = $1)")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, q)
		n := len(args)
		where = append(where, fmt.Sprintf("(s.name ILIKE '%%' || $%d || '%%' OR s.description ILIKE '%%' || $%d || '%%')", n, n))
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM segments s WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count segments: %w", err)
	}
	limit, offset := f.Limit, max(0, f.Offset)
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit, offset)
	rows, err := db.Query(ctx,
		`SELECT `+prefixedColumns(segmentColumns, "s.")+`
		   FROM segments s
		  WHERE `+cond+`
		  ORDER BY `+segmentOrderSQL(f.Sort)+`
		  LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list segments: %w", err)
	}
	out, err := pgCollectRows(rows, scanSegmentRow, "scan segment", "iterate segments")
	return out, total, err
}

// segmentOrderSQL maps a sort to its ORDER BY; the viewer's effort count
// sort uses the bound viewer parameter $1.
func segmentOrderSQL(sort domain.SegmentSort) string {
	switch sort {
	case domain.SegmentSortDistanceDesc:
		return "s.distance_m DESC, s.name ASC"
	case domain.SegmentSortElevationDesc:
		return "s.elevation_gain_m DESC NULLS LAST, s.name ASC"
	case domain.SegmentSortEffortCountDesc:
		return "(SELECT count(*) FROM segment_efforts e WHERE e.segment_id = s.id AND e.user_id = $1) DESC, s.name ASC"
	case domain.SegmentSortCreatedAtDesc:
		return "s.created_at DESC"
	}
	return "s.name ASC"
}

func (r *SegmentRepo) SearchSegmentsInBBox(ctx context.Context, viewer domain.UserID, box port.BoundingBox, types []domain.ActivityType, includeInstance bool, limit int) ([]domain.Segment, error) {
	db := dbtx(ctx, r.pool)
	if limit <= 0 {
		limit = 100
	}
	visible := segmentVisibleSQL("$1")
	if !includeInstance {
		visible = `((s.source = 'native' AND s.owner_user_id = $1)
			OR (s.source = 'external' AND s.external_account_id IN (SELECT id FROM external_accounts WHERE user_id = $1)))`
	}
	rows, err := db.Query(ctx,
		`SELECT `+prefixedColumns(segmentColumns, "s.")+`
		   FROM segments s
		  WHERE ST_Intersects(s.bbox, ST_GeogFromText($2))
		    AND (cardinality($3::text[]) = 0 OR s.activity_type = ANY($3))
		    AND `+visible+`
		  ORDER BY s.distance_m DESC
		  LIMIT $4`,
		viewer.UUID(), bboxToPolygonWKT(box), stringsOf(types), limit)
	if err != nil {
		return nil, fmt.Errorf("search segments: %w", err)
	}
	return pgCollectRows(rows, scanSegmentRow, "scan segment", "iterate segments")
}

func (r *SegmentRepo) ListSegmentEffortsFiltered(ctx context.Context, segmentID domain.SegmentID, q domain.SegmentEffortQuery) ([]domain.SegmentEffort, error) {
	db := dbtx(ctx, r.pool)
	limit, offset := q.Limit, max(0, q.Offset)
	if limit <= 0 {
		limit = 50
	}
	order := "start_time DESC"
	if q.FastestFirst {
		order = "elapsed_s ASC, start_time ASC"
	}
	var userParam any
	if q.UserID != nil {
		userParam = q.UserID.UUID()
	}
	rows, err := db.Query(ctx,
		`SELECT `+segmentEffortColumns+`
		   FROM segment_efforts
		  WHERE segment_id = $1
		    AND ($2::uuid IS NULL OR user_id = $2)
		    AND ($3::timestamptz IS NULL OR start_time >= $3)
		    AND ($4::timestamptz IS NULL OR start_time < $4)
		  ORDER BY `+order+`
		  LIMIT $5 OFFSET $6`,
		segmentID.UUID(), userParam, nullableTime(q.From), nullableTime(q.To), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list segment efforts: %w", err)
	}
	return pgCollectRows(rows, scanSegmentEffortRow, "scan effort", "iterate efforts")
}

const leaderboardSQL = `
	WITH best AS (
		SELECT DISTINCT ON (e.user_id) e.id, e.user_id, e.activity_id, e.elapsed_s, e.start_time
		  FROM segment_efforts e
		 WHERE e.segment_id = $1
		   AND ($2::timestamptz IS NULL OR e.start_time >= $2)
		   AND ($3::timestamptz IS NULL OR e.start_time < $3)
		 ORDER BY e.user_id, e.elapsed_s ASC, e.start_time ASC
	), ranked AS (
		SELECT b.*, rank() OVER (ORDER BY b.elapsed_s ASC, b.start_time ASC) AS rnk,
		       u.display_name, COALESCE(u.avatar_url, '') AS avatar_url
		  FROM best b JOIN users u ON u.id = b.user_id
	)`

func (r *SegmentRepo) SegmentLeaderboard(ctx context.Context, segmentID domain.SegmentID, viewer domain.UserID, q domain.LeaderboardQuery) ([]domain.LeaderboardEntry, *domain.LeaderboardEntry, error) {
	db := dbtx(ctx, r.pool)
	limit, offset := q.Limit, max(0, q.Offset)
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(ctx, leaderboardSQL+`
		SELECT rnk, user_id, display_name, avatar_url, elapsed_s, start_time, id, activity_id
		  FROM ranked ORDER BY rnk LIMIT $4 OFFSET $5`,
		segmentID.UUID(), nullableTime(q.From), nullableTime(q.To), limit, offset)
	if err != nil {
		return nil, nil, fmt.Errorf("segment leaderboard: %w", err)
	}
	entries, err := pgCollectRows(rows, scanLeaderboardEntry, "scan leaderboard entry", "iterate leaderboard")
	if err != nil {
		return nil, nil, err
	}
	mine, err := scanLeaderboardEntry(db.QueryRow(ctx, leaderboardSQL+`
		SELECT rnk, user_id, display_name, avatar_url, elapsed_s, start_time, id, activity_id
		  FROM ranked WHERE user_id = $4`,
		segmentID.UUID(), nullableTime(q.From), nullableTime(q.To), viewer.UUID()))
	if err != nil {
		return entries, nil, nil //nolint:nilerr // no row for the viewer is the common case
	}
	return entries, &mine, nil
}

func scanLeaderboardEntry(row rowScanner) (domain.LeaderboardEntry, error) {
	var (
		e             domain.LeaderboardEntry
		uid, eid, aid uuid.UUID
		rank          int64
	)
	if err := row.Scan(&rank, &uid, &e.DisplayName, &e.AvatarURL, &e.ElapsedS, &e.StartTime, &eid, &aid); err != nil {
		return domain.LeaderboardEntry{}, err
	}
	e.Rank, e.UserID, e.EffortID, e.ActivityID = int(rank), domain.UserID(uid), domain.SegmentEffortID(eid), domain.ActivityID(aid)
	return e, nil
}

// stringsOf renders a slice of string-kinded values for an ANY($n) bind.
func stringsOf[T ~string](in []T) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, string(v))
	}
	return out
}
