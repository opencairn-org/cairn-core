package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ShareLinkRepo implements port.ShareLinkRepo over activity_share_links.
type ShareLinkRepo struct{ pool *pgxpool.Pool }

func NewShareLinkRepo(pool *pgxpool.Pool) *ShareLinkRepo { return &ShareLinkRepo{pool: pool} }

func (r *ShareLinkRepo) Create(ctx context.Context, l domain.ShareLink) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`INSERT INTO activity_share_links (token, activity_id, created_by) VALUES ($1,$2,$3)`,
		l.Token, l.ActivityID.UUID(), l.CreatedBy.UUID())
	if err != nil {
		return fmt.Errorf("create share link: %w", err)
	}
	return nil
}

func (r *ShareLinkRepo) GetActive(ctx context.Context, token string) (domain.ShareLink, error) {
	db := dbtx(ctx, r.pool)
	var (
		l       domain.ShareLink
		actID   uuid.UUID
		creator uuid.UUID
	)
	err := db.QueryRow(ctx,
		`SELECT token, activity_id, created_by, created_at, revoked_at
		   FROM activity_share_links WHERE token = $1 AND revoked_at IS NULL`,
		token).Scan(&l.Token, &actID, &creator, &l.CreatedAt, &l.RevokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ShareLink{}, domain.ErrNotFound
		}
		return domain.ShareLink{}, fmt.Errorf("get share link: %w", err)
	}
	l.ActivityID = domain.ActivityID(actID)
	l.CreatedBy = domain.UserID(creator)
	return l, nil
}

func (r *ShareLinkRepo) ListForActivity(ctx context.Context, activityID domain.ActivityID) ([]domain.ShareLink, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT token, activity_id, created_by, created_at, revoked_at
		   FROM activity_share_links WHERE activity_id = $1 ORDER BY created_at DESC`,
		activityID.UUID())
	if err != nil {
		return nil, fmt.Errorf("list share links: %w", err)
	}
	defer rows.Close()
	var out []domain.ShareLink
	for rows.Next() {
		var l domain.ShareLink
		var actID, creator uuid.UUID
		if err := rows.Scan(&l.Token, &actID, &creator, &l.CreatedAt, &l.RevokedAt); err != nil {
			return nil, err
		}
		l.ActivityID = domain.ActivityID(actID)
		l.CreatedBy = domain.UserID(creator)
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *ShareLinkRepo) Revoke(ctx context.Context, token string, owner domain.UserID) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE activity_share_links SET revoked_at = now()
		   WHERE token = $1 AND created_by = $2 AND revoked_at IS NULL`,
		token, owner.UUID())
	if err != nil {
		return fmt.Errorf("revoke share link: %w", err)
	}
	return nil
}

// EngagementRepo implements port.EngagementRepo (kudos + comments).
type EngagementRepo struct{ pool *pgxpool.Pool }

func NewEngagementRepo(pool *pgxpool.Pool) *EngagementRepo { return &EngagementRepo{pool: pool} }

func (r *EngagementRepo) AddKudos(ctx context.Context, a domain.ActivityID, u domain.UserID) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`INSERT INTO activity_kudos (activity_id, user_id) VALUES ($1,$2)
		 ON CONFLICT DO NOTHING`, a.UUID(), u.UUID())
	if err != nil {
		return fmt.Errorf("add kudos: %w", err)
	}
	return nil
}

func (r *EngagementRepo) RemoveKudos(ctx context.Context, a domain.ActivityID, u domain.UserID) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`DELETE FROM activity_kudos WHERE activity_id = $1 AND user_id = $2`, a.UUID(), u.UUID())
	if err != nil {
		return fmt.Errorf("remove kudos: %w", err)
	}
	return nil
}

func (r *EngagementRepo) CountKudos(ctx context.Context, a domain.ActivityID) (int, error) {
	db := dbtx(ctx, r.pool)
	var n int
	err := db.QueryRow(ctx, `SELECT count(*) FROM activity_kudos WHERE activity_id = $1`, a.UUID()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count kudos: %w", err)
	}
	return n, nil
}

func (r *EngagementRepo) HasKudos(ctx context.Context, a domain.ActivityID, u domain.UserID) (bool, error) {
	db := dbtx(ctx, r.pool)
	var exists bool
	err := db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM activity_kudos WHERE activity_id=$1 AND user_id=$2)`,
		a.UUID(), u.UUID()).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("has kudos: %w", err)
	}
	return exists, nil
}

func (r *EngagementRepo) ListKudosers(ctx context.Context, a domain.ActivityID, limit int) ([]domain.UserID, error) {
	if limit <= 0 {
		limit = 100
	}
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT user_id FROM activity_kudos WHERE activity_id=$1 ORDER BY created_at DESC LIMIT $2`,
		a.UUID(), limit)
	if err != nil {
		return nil, fmt.Errorf("list kudosers: %w", err)
	}
	defer rows.Close()
	var out []domain.UserID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, domain.UserID(id))
	}
	return out, rows.Err()
}

// RemoteKudoser is a federated actor who liked a local activity, with a display
// handle resolved from the cached actor record (falls back to the actor URL).
// RemoteKudoser is domain.RemoteKudoser (kept as an alias for callers).
type RemoteKudoser = domain.RemoteKudoser

func (r *EngagementRepo) AddRemoteKudos(ctx context.Context, a domain.ActivityID, remoteActorID, likeActivityID string) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`INSERT INTO activity_remote_kudos (activity_id, remote_actor_id, like_activity_id)
		 VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
		a.UUID(), remoteActorID, likeActivityID)
	if err != nil {
		return fmt.Errorf("add remote kudos: %w", err)
	}
	return nil
}

func (r *EngagementRepo) RemoveRemoteKudos(ctx context.Context, a domain.ActivityID, remoteActorID string) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`DELETE FROM activity_remote_kudos WHERE activity_id=$1 AND remote_actor_id=$2`,
		a.UUID(), remoteActorID)
	if err != nil {
		return fmt.Errorf("remove remote kudos: %w", err)
	}
	return nil
}

func (r *EngagementRepo) CountRemoteKudos(ctx context.Context, a domain.ActivityID) (int, error) {
	db := dbtx(ctx, r.pool)
	var n int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM activity_remote_kudos WHERE activity_id=$1`, a.UUID()).Scan(&n); err != nil {
		return 0, fmt.Errorf("count remote kudos: %w", err)
	}
	return n, nil
}

// remoteHandleExpr is the SQL that renders a left-joined `federation_actors fa`
// as a friendly @user@domain, falling back to the raw actor URL (in
// <rowAlias>.remote_actor_id) when the actor doc hasn't been fetched. Shared by
// the kudos + comment list queries so the handle format stays identical.
func remoteHandleExpr(rowAlias string) string {
	return `COALESCE(NULLIF('@' || fa.preferred_username || '@' || fa.domain, '@@'), ` + rowAlias + `.remote_actor_id)`
}

func (r *EngagementRepo) ListRemoteKudosers(ctx context.Context, a domain.ActivityID, limit int) ([]RemoteKudoser, error) {
	if limit <= 0 {
		limit = 100
	}
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT k.remote_actor_id, `+remoteHandleExpr("k")+`
		   FROM activity_remote_kudos k
		   LEFT JOIN federation_actors fa ON fa.actor_id = k.remote_actor_id
		  WHERE k.activity_id = $1
		  ORDER BY k.created_at DESC
		  LIMIT $2`,
		a.UUID(), limit)
	if err != nil {
		return nil, fmt.Errorf("list remote kudosers: %w", err)
	}
	defer rows.Close()
	var out []RemoteKudoser
	for rows.Next() {
		var rk RemoteKudoser
		if err := rows.Scan(&rk.ActorID, &rk.Handle); err != nil {
			return nil, err
		}
		out = append(out, rk)
	}
	return out, rows.Err()
}

// RemoteComment is a federated reply (Create{Note}) on a local activity, with a
// display handle resolved from the cached actor.
// RemoteComment is domain.RemoteComment (kept as an alias for callers).
type RemoteComment = domain.RemoteComment

func (r *EngagementRepo) AddRemoteComment(ctx context.Context, a domain.ActivityID, remoteActorID, noteAPID, body string) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`INSERT INTO activity_remote_comments (activity_id, remote_actor_id, note_ap_id, body)
		 VALUES ($1,$2,$3,$4) ON CONFLICT (activity_id, note_ap_id) DO NOTHING`,
		a.UUID(), remoteActorID, noteAPID, body)
	if err != nil {
		return fmt.Errorf("add remote comment: %w", err)
	}
	return nil
}

// DeleteRemoteComment soft-deletes a federated comment by its Note id, scoped to
// the signer so a remote can only delete its own reply.
func (r *EngagementRepo) DeleteRemoteComment(ctx context.Context, remoteActorID, noteAPID string) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE activity_remote_comments SET deleted_at = now()
		   WHERE note_ap_id = $1 AND remote_actor_id = $2 AND deleted_at IS NULL`,
		noteAPID, remoteActorID)
	if err != nil {
		return fmt.Errorf("delete remote comment: %w", err)
	}
	return nil
}

func (r *EngagementRepo) ListRemoteComments(ctx context.Context, a domain.ActivityID, limit int) ([]RemoteComment, error) {
	if limit <= 0 {
		limit = 200
	}
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT c.id, c.remote_actor_id, `+remoteHandleExpr("c")+`, c.body, c.created_at
		   FROM activity_remote_comments c
		   LEFT JOIN federation_actors fa ON fa.actor_id = c.remote_actor_id
		  WHERE c.activity_id = $1 AND c.deleted_at IS NULL
		  ORDER BY c.created_at
		  LIMIT $2`,
		a.UUID(), limit)
	if err != nil {
		return nil, fmt.Errorf("list remote comments: %w", err)
	}
	defer rows.Close()
	var out []RemoteComment
	for rows.Next() {
		var rc RemoteComment
		var id uuid.UUID
		if err := rows.Scan(&id, &rc.ActorID, &rc.Handle, &rc.Body, &rc.CreatedAt); err != nil {
			return nil, err
		}
		rc.ID = id.String()
		out = append(out, rc)
	}
	return out, rows.Err()
}

// DeleteRemoteKudosFromActor removes every federated kudos by remoteActorID on
// activities owned by ownerUserID — used when that actor leaves the network
// (inbound self-Delete) so departed actors don't keep inflating kudos counts.
func (r *EngagementRepo) DeleteRemoteKudosFromActor(ctx context.Context, ownerUserID domain.UserID, remoteActorID string) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`DELETE FROM activity_remote_kudos
		   WHERE remote_actor_id = $2
		     AND activity_id IN (SELECT id FROM activities WHERE user_id = $1)`,
		ownerUserID.UUID(), remoteActorID)
	if err != nil {
		return fmt.Errorf("delete remote kudos from actor: %w", err)
	}
	return nil
}

// DeleteRemoteCommentsFromActor soft-deletes every federated comment by
// remoteActorID on activities owned by ownerUserID (actor self-Delete cleanup).
func (r *EngagementRepo) DeleteRemoteCommentsFromActor(ctx context.Context, ownerUserID domain.UserID, remoteActorID string) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE activity_remote_comments SET deleted_at = now()
		   WHERE remote_actor_id = $2 AND deleted_at IS NULL
		     AND activity_id IN (SELECT id FROM activities WHERE user_id = $1)`,
		ownerUserID.UUID(), remoteActorID)
	if err != nil {
		return fmt.Errorf("delete remote comments from actor: %w", err)
	}
	return nil
}

func (r *EngagementRepo) AddComment(ctx context.Context, c domain.Comment) (domain.CommentID, error) {
	db := dbtx(ctx, r.pool)
	var id uuid.UUID
	err := db.QueryRow(ctx,
		`INSERT INTO activity_comments (activity_id, user_id, body) VALUES ($1,$2,$3) RETURNING id`,
		c.ActivityID.UUID(), c.UserID.UUID(), c.Body).Scan(&id)
	if err != nil {
		return domain.CommentID{}, fmt.Errorf("add comment: %w", err)
	}
	return domain.CommentID(id), nil
}

func (r *EngagementRepo) ListComments(ctx context.Context, a domain.ActivityID, limit, offset int) ([]domain.Comment, error) {
	if limit <= 0 {
		limit = 50
	}
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT id, activity_id, user_id, body, created_at
		   FROM activity_comments
		   WHERE activity_id=$1 AND deleted_at IS NULL
		   ORDER BY created_at ASC LIMIT $2 OFFSET $3`,
		a.UUID(), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()
	var out []domain.Comment
	for rows.Next() {
		var c domain.Comment
		var id, actID, uid uuid.UUID
		if err := rows.Scan(&id, &actID, &uid, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.ID = domain.CommentID(id)
		c.ActivityID = domain.ActivityID(actID)
		c.UserID = domain.UserID(uid)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *EngagementRepo) CountComments(ctx context.Context, a domain.ActivityID) (int, error) {
	db := dbtx(ctx, r.pool)
	var n int
	err := db.QueryRow(ctx,
		`SELECT count(*) FROM activity_comments WHERE activity_id=$1 AND deleted_at IS NULL`,
		a.UUID()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count comments: %w", err)
	}
	return n, nil
}

func (r *EngagementRepo) DeleteComment(ctx context.Context, id domain.CommentID, requester domain.UserID) error {
	db := dbtx(ctx, r.pool)
	// Permitted for the comment author OR the owner of the activity the
	// comment is on — resolved in a single statement so the handler needs no
	// extra ownership lookup.
	_, err := db.Exec(ctx,
		`UPDATE activity_comments c SET deleted_at = now()
		   WHERE c.id = $1 AND c.deleted_at IS NULL
		     AND (c.user_id = $2
		          OR EXISTS (SELECT 1 FROM activities a
		                       WHERE a.id = c.activity_id AND a.user_id = $2))`,
		id.UUID(), requester.UUID())
	if err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	return nil
}
