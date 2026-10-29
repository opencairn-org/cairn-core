package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// OAuthServerRepo implements port.OAuthServerRepo (migration 00061).
type OAuthServerRepo struct {
	pool *pgxpool.Pool
}

func NewOAuthServerRepo(pool *pgxpool.Pool) *OAuthServerRepo {
	return &OAuthServerRepo{pool: pool}
}

// ---- clients ----

func (r *OAuthServerRepo) CreateClient(ctx context.Context, c domain.OAuthClient) error {
	var createdBy *uuid.UUID
	if c.CreatedBy != nil {
		u := c.CreatedBy.UUID()
		createdBy = &u
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO oauth_clients
			(client_id, secret_hash, name, redirect_uris, grant_types, scopes, token_auth, is_dynamic, created_by,
			 software_id, software_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		c.ClientID, c.SecretHash, c.Name, c.RedirectURIs, c.GrantTypes, c.Scopes,
		c.TokenEndpointAuthMethod, c.IsDynamic, createdBy, c.SoftwareID, c.SoftwareVersion)
	if err != nil {
		return fmt.Errorf("create oauth client: %w", err)
	}
	return nil
}

func (r *OAuthServerRepo) GetClient(ctx context.Context, clientID string) (domain.OAuthClient, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT client_id, secret_hash, name, redirect_uris, grant_types, scopes, token_auth, is_dynamic, created_by, created_at,
		       software_id, software_version
		FROM oauth_clients WHERE client_id = $1`, clientID)
	var (
		c         domain.OAuthClient
		createdBy *uuid.UUID
	)
	err := row.Scan(&c.ClientID, &c.SecretHash, &c.Name, &c.RedirectURIs, &c.GrantTypes,
		&c.Scopes, &c.TokenEndpointAuthMethod, &c.IsDynamic, &createdBy, &c.CreatedAt,
		&c.SoftwareID, &c.SoftwareVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.OAuthClient{}, fmt.Errorf("oauth client: %w", domain.ErrNotFound)
	}
	if err != nil {
		return domain.OAuthClient{}, err
	}
	if createdBy != nil {
		uid := domain.UserID(*createdBy)
		c.CreatedBy = &uid
	}
	return c, nil
}

// ---- authorization codes ----

func (r *OAuthServerRepo) CreateAuthCode(ctx context.Context, code domain.OAuthAuthorizationCode, codeHash []byte) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO oauth_authorization_codes
			(code_hash, client_id, user_id, redirect_uri, scope, code_challenge, code_challenge_method, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		codeHash, code.ClientID, code.UserID.UUID(), code.RedirectURI, code.Scope,
		code.CodeChallenge, code.CodeChallengeMethod, code.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create auth code: %w", err)
	}
	return nil
}

func (r *OAuthServerRepo) ConsumeAuthCode(ctx context.Context, codeHash []byte, now time.Time) (domain.OAuthAuthorizationCode, error) {
	// Single-statement atomic consume: only succeeds if unconsumed + unexpired.
	row := r.pool.QueryRow(ctx, `
		UPDATE oauth_authorization_codes
		   SET consumed_at = $2
		 WHERE code_hash = $1 AND consumed_at IS NULL AND expires_at > $2
		RETURNING client_id, user_id, redirect_uri, scope, code_challenge, code_challenge_method, expires_at`,
		codeHash, now)
	var (
		c   domain.OAuthAuthorizationCode
		uid uuid.UUID
	)
	err := row.Scan(&c.ClientID, &uid, &c.RedirectURI, &c.Scope, &c.CodeChallenge, &c.CodeChallengeMethod, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.OAuthAuthorizationCode{}, fmt.Errorf("auth code: %w", domain.ErrNotFound)
	}
	if err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	c.UserID = domain.UserID(uid)
	consumed := now
	c.ConsumedAt = &consumed
	return c, nil
}

// ---- access tokens ----

func (r *OAuthServerRepo) CreateAccessToken(ctx context.Context, t domain.OAuthAccessToken, tokenHash []byte) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO oauth_access_tokens (token_hash, client_id, user_id, scope, expires_at)
		VALUES ($1,$2,$3,$4,$5)`,
		tokenHash, t.ClientID, t.UserID.UUID(), t.Scope, t.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create access token: %w", err)
	}
	return nil
}

func (r *OAuthServerRepo) FindAccessToken(ctx context.Context, tokenHash []byte) (domain.OAuthAccessToken, error) {
	return r.oauthASFindToken(ctx, `
		SELECT client_id, user_id, scope, expires_at, revoked_at
		FROM oauth_access_tokens WHERE token_hash = $1`, "access token", tokenHash)
}

// oauthASFindToken loads one access/refresh token row; label prefixes the not-found error.
func (r *OAuthServerRepo) oauthASFindToken(ctx context.Context, query, label string, tokenHash []byte) (domain.OAuthAccessToken, error) {
	row := r.pool.QueryRow(ctx, query, tokenHash)
	var (
		t         domain.OAuthAccessToken
		uid       uuid.UUID
		revokedAt *time.Time
	)
	err := row.Scan(&t.ClientID, &uid, &t.Scope, &t.ExpiresAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.OAuthAccessToken{}, fmt.Errorf("%s: %w", label, domain.ErrNotFound)
	}
	if err != nil {
		return domain.OAuthAccessToken{}, err
	}
	t.UserID = domain.UserID(uid)
	t.RevokedAt = revokedAt
	return t, nil
}

func (r *OAuthServerRepo) RevokeAccessToken(ctx context.Context, tokenHash []byte) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE oauth_access_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash)
	return err
}

// ---- refresh tokens ----

func (r *OAuthServerRepo) CreateRefreshToken(ctx context.Context, t domain.OAuthRefreshToken, tokenHash []byte) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO oauth_refresh_tokens (token_hash, client_id, user_id, scope, expires_at)
		VALUES ($1,$2,$3,$4,$5)`,
		tokenHash, t.ClientID, t.UserID.UUID(), t.Scope, t.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create refresh token: %w", err)
	}
	return nil
}

func (r *OAuthServerRepo) FindRefreshToken(ctx context.Context, tokenHash []byte) (domain.OAuthRefreshToken, error) {
	t, err := r.oauthASFindToken(ctx, `
		SELECT client_id, user_id, scope, expires_at, revoked_at
		FROM oauth_refresh_tokens WHERE token_hash = $1`, "refresh token", tokenHash)
	if err != nil {
		return domain.OAuthRefreshToken{}, err
	}
	return domain.OAuthRefreshToken(t), nil
}

func (r *OAuthServerRepo) RevokeRefreshToken(ctx context.Context, tokenHash []byte) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE oauth_refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash)
	return err
}

// ---- authorized apps ----

func (r *OAuthServerRepo) ListGrantsForUser(ctx context.Context, userID domain.UserID, now time.Time) ([]domain.OAuthGrant, error) {
	rows, err := r.pool.Query(ctx, `
		WITH live AS (
			SELECT client_id, scope, created_at FROM oauth_access_tokens
			 WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > $2
			UNION ALL
			SELECT client_id, scope, created_at FROM oauth_refresh_tokens
			 WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > $2
		)
		SELECT l.client_id, COALESCE(c.name, l.client_id), COALESCE(c.software_id, ''), COALESCE(c.software_version, ''),
		       array_agg(DISTINCT l.scope), min(l.created_at), max(l.created_at), count(*)
		  FROM live l LEFT JOIN oauth_clients c ON c.client_id = l.client_id
		 GROUP BY l.client_id, c.name, c.software_id, c.software_version
		 ORDER BY max(l.created_at) DESC`, userID.UUID(), now)
	if err != nil {
		return nil, fmt.Errorf("list oauth grants: %w", err)
	}
	defer rows.Close()
	out := []domain.OAuthGrant{}
	for rows.Next() {
		var (
			g      domain.OAuthGrant
			scopes []string
			n      int64
		)
		if err := rows.Scan(&g.ClientID, &g.ClientName, &g.SoftwareID, &g.SoftwareVersion, &scopes, &g.GrantedAt, &g.LastIssuedAt, &n); err != nil {
			return nil, fmt.Errorf("scan oauth grant: %w", err)
		}
		g.Scopes = mergeScopeStrings(scopes)
		g.ActiveTokens = int(n)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (r *OAuthServerRepo) RevokeClientForUser(ctx context.Context, userID domain.UserID, clientID string) (int, error) {
	n := 0
	for _, table := range []string{"oauth_access_tokens", "oauth_refresh_tokens"} {
		tag, err := r.pool.Exec(ctx,
			`UPDATE `+table+` SET revoked_at = now() WHERE user_id = $1 AND client_id = $2 AND revoked_at IS NULL`,
			userID.UUID(), clientID)
		if err != nil {
			return n, fmt.Errorf("revoke %s: %w", table, err)
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

// mergeScopeStrings unions space-separated scope strings into a sorted set.
func mergeScopeStrings(in []string) []string {
	set := map[string]struct{}{}
	for _, s := range in {
		for _, sc := range strings.Fields(s) {
			set[sc] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for sc := range set {
		out = append(out, sc)
	}
	sort.Strings(out)
	return out
}
