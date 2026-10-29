package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// UserRepo implements port.UserRepo.
//
// Conventions:
//   - Usernames and emails are stored as citext; we always pass already-
//     normalised (lowercased, trimmed) values to keep diffs and logs stable.
//   - PasswordHash lives on Credentials, kept separate from User on read
//     paths. CreateUser handles the joint write in one transaction.
//   - pgx unique-violation errors are mapped onto port.ErrUsernameTaken /
//     port.ErrEmailTaken via the constraint name.
type UserRepo struct {
	pool *pgxpool.Pool
}

func NewUserRepo(pool *pgxpool.Pool) *UserRepo {
	return &UserRepo{pool: pool}
}

// ---------------------------------------------------------------------------
// userColumns is the canonical SELECT list. scanUserRow depends on this order.
// ---------------------------------------------------------------------------

const userColumns = `
	id, username, email, email_verified, display_name, avatar_url,
	locale, timezone, units,
	date_format, time_format,
	role, status, status_reason,
	must_change_password,
	created_at, updated_at, last_login_at,
	profile_is_public, passkey_second_factor
`

// ---------------------------------------------------------------------------
// CreateUser
// ---------------------------------------------------------------------------

func (r *UserRepo) CreateUser(ctx context.Context, u domain.User, cred domain.Credentials) error {
	if err := domain.ValidateUserShape(u); err != nil {
		return fmt.Errorf("validate user: %w", err)
	}
	db := dbtx(ctx, r.pool)

	var (
		gotID                uuid.UUID
		createdAt, updatedAt time.Time
	)
	err := db.QueryRow(ctx, userCreateSQL, userCreateArgs(u, cred)...).Scan(&gotID, &createdAt, &updatedAt)

	if err != nil {
		return mapUserUniqueViolation(err)
	}
	_ = gotID
	_ = createdAt
	_ = updatedAt
	return nil
}

// userCreateSQL inserts one user and returns its generated columns.
const userCreateSQL = `
		INSERT INTO users (
			id, username, email, email_verified,
			password_hash, display_name, avatar_url,
			locale, timezone, units,
			role, status, status_reason,
			must_change_password
		) VALUES (
			COALESCE($1, uuidv7()), $2, $3, $4,
			$5, $6, $7,
			$8, $9, $10,
			$11, $12, $13,
			$14
		)
		RETURNING id, created_at, updated_at
	`

// userCreateArgs builds the userCreateSQL bind parameters, applying locale/unit/tz defaults.
func userCreateArgs(u domain.User, cred domain.Credentials) []any {
	units := u.Units
	if units == "" {
		units = domain.UserUnitsMetric
	}
	locale := u.Locale
	if locale == "" {
		locale = "en"
	}
	tz := u.Timezone
	if tz == "" {
		tz = "UTC"
	}

	var idParam any
	if u.ID != (domain.UserID{}) {
		idParam = u.ID.UUID()
	}

	var passwordParam any
	if cred.PasswordHash != "" {
		passwordParam = cred.PasswordHash
	}

	var avatarParam any
	if u.AvatarURL != "" {
		avatarParam = u.AvatarURL
	}

	var statusReasonParam any
	if u.StatusReason != "" {
		statusReasonParam = u.StatusReason
	}

	return []any{
		idParam, u.Username, u.Email, u.EmailVerified,
		passwordParam, u.DisplayName, avatarParam,
		locale, tz, string(units),
		string(u.Role), string(u.Status), statusReasonParam,
		u.MustChangePassword,
	}
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

func (r *UserRepo) GetUser(ctx context.Context, id domain.UserID) (domain.User, error) {
	db := dbtx(ctx, r.pool)
	row := db.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1 AND status != 'deleted'`,
		id.UUID(),
	)
	u, err := scanUserRow(row)
	return wrapNotFound(u, err, fmt.Sprintf("get user %s", id))
}

// SetProfilePublic toggles the user's public-profile opt-in.
func (r *UserRepo) SetProfilePublic(ctx context.Context, id domain.UserID, public bool) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE users SET profile_is_public = $2, updated_at = now() WHERE id = $1`,
		id.UUID(), public)
	if err != nil {
		return fmt.Errorf("set profile public: %w", err)
	}
	return nil
}

// SetFederationEnabled toggles the user's per-user ActivityPub opt-in.
func (r *UserRepo) SetFederationEnabled(ctx context.Context, id domain.UserID, enabled bool) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE users SET federation_enabled = $2, updated_at = now() WHERE id = $1`,
		id.UUID(), enabled)
	if err != nil {
		return fmt.Errorf("set federation enabled: %w", err)
	}
	return nil
}

// IsFederationEnabled reports whether the user opted into federation.
func (r *UserRepo) IsFederationEnabled(ctx context.Context, id domain.UserID) (bool, error) {
	db := dbtx(ctx, r.pool)
	var enabled bool
	if err := db.QueryRow(ctx,
		`SELECT federation_enabled FROM users WHERE id = $1`, id.UUID()).Scan(&enabled); err != nil {
		return false, fmt.Errorf("get federation enabled: %w", err)
	}
	return enabled, nil
}

// ListAdmins returns every active admin user — recipients for instance-level
// notifications (e.g. a worker going offline).
func (r *UserRepo) ListAdmins(ctx context.Context) ([]domain.User, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT `+userColumns+` FROM users
		  WHERE role = 'admin' AND status = 'active'
		  ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list admins: %w", err)
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan admin row: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *UserRepo) GetUserByUsername(ctx context.Context, username string) (domain.User, error) {
	db := dbtx(ctx, r.pool)
	row := db.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE username = $1 AND status != 'deleted'`,
		username,
	)
	u, err := scanUserRow(row)
	return wrapNotFound(u, err, fmt.Sprintf("get user by username %q", username))
}

func (r *UserRepo) GetUserByEmail(ctx context.Context, email string) (domain.User, error) {
	db := dbtx(ctx, r.pool)
	row := db.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = $1 AND status != 'deleted'`,
		email,
	)
	u, err := scanUserRow(row)
	return wrapNotFound(u, err, fmt.Sprintf("get user by email %q", email))
}

// GetCredentialsByLogin resolves a username-or-email login. We rely on
// citext on username and email, plus the difference in valid characters
// ('@' is invalid in usernames), to disambiguate.
func (r *UserRepo) GetCredentialsByLogin(
	ctx context.Context,
	login string,
) (domain.User, domain.Credentials, error) {
	db := dbtx(ctx, r.pool)

	const q = `
		SELECT ` + userColumns + `, COALESCE(password_hash, '')
		FROM users
		WHERE (username = $1 OR email = $1) AND status != 'deleted'
		LIMIT 1
	`

	row := db.QueryRow(ctx, q, login)
	u, passwordHash, err := scanUserRowWithCredentials(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, domain.Credentials{},
			fmt.Errorf("get credentials by login: %w", domain.ErrNotFound)
	}
	if err != nil {
		return domain.User{}, domain.Credentials{},
			fmt.Errorf("get credentials by login: %w", err)
	}
	return u, domain.Credentials{UserID: u.ID, PasswordHash: passwordHash}, nil
}

// ---------------------------------------------------------------------------
// Updates
// ---------------------------------------------------------------------------

func (r *UserRepo) UpdateUserRole(
	ctx context.Context,
	id domain.UserID,
	role domain.UserRole,
) error {
	if !role.Valid() {
		return fmt.Errorf("update user role: invalid role %q", role)
	}
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE users SET role = $1 WHERE id = $2 AND status != 'deleted'`,
		string(role), id.UUID(),
	)
	if err != nil {
		return fmt.Errorf("update user role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update user role %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

// UpdateUserPreferences sets the display-format preferences (units + date/time
// format). Empty strings clear back to the locale default.
func (r *UserRepo) UpdateUserPreferences(
	ctx context.Context,
	id domain.UserID,
	units, dateFormat, timeFormat string,
) error {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE users
		    SET units = $2, date_format = $3, time_format = $4, updated_at = now()
		  WHERE id = $1 AND status != 'deleted'`,
		id.UUID(), units, dateFormat, timeFormat,
	)
	if err != nil {
		return fmt.Errorf("update user preferences: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update user preferences %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

func (r *UserRepo) UpdateUserStatus(
	ctx context.Context,
	id domain.UserID,
	status domain.UserStatus,
	reason string,
) error {
	if !status.Valid() {
		return fmt.Errorf("update user status: invalid status %q", status)
	}
	var reasonParam any
	if reason != "" {
		reasonParam = reason
	}

	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE users SET status = $1, status_reason = $2 WHERE id = $3`,
		string(status), reasonParam, id.UUID(),
	)
	if err != nil {
		return fmt.Errorf("update user status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update user status %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

func (r *UserRepo) UpdateProfile(ctx context.Context, id domain.UserID, p port.ProfileUpdate) error {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE users
		    SET display_name = $2, avatar_url = $3, locale = $4, timezone = $5, units = $6,
		        updated_at = now()
		  WHERE id = $1 AND status != 'deleted'`,
		id.UUID(), p.DisplayName, nullIfEmpty(p.AvatarURL), p.Locale, p.Timezone, p.Units,
	)
	if err != nil {
		return fmt.Errorf("update profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update profile %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

func (r *UserRepo) UpdatePassword(
	ctx context.Context,
	id domain.UserID,
	encodedHash string,
) error {
	var hashParam any
	if encodedHash != "" {
		hashParam = encodedHash
	}

	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE users
		   SET password_hash = $1,
		       must_change_password = false
		 WHERE id = $2 AND status != 'deleted'`,
		hashParam, id.UUID(),
	)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update password %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

// UpdateEmailVerified sets users.email_verified.
func (r *UserRepo) SetPasskeySecondFactor(ctx context.Context, id domain.UserID, enabled bool) error {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE users SET passkey_second_factor = $2, updated_at = now() WHERE id = $1 AND status != 'deleted'`,
		id.UUID(), enabled)
	if err != nil {
		return fmt.Errorf("set passkey second factor: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set passkey second factor %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

func (r *UserRepo) UpdateEmailVerified(ctx context.Context, id domain.UserID, verified bool) error {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE users SET email_verified = $1 WHERE id = $2 AND status != 'deleted'`,
		verified, id.UUID(),
	)
	if err != nil {
		return fmt.Errorf("update email_verified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update email_verified %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Aggregates / listing
// ---------------------------------------------------------------------------

func (r *UserRepo) CountAdmins(ctx context.Context) (int, error) {
	db := dbtx(ctx, r.pool)
	var n int
	err := db.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE role = 'admin' AND status = 'active'`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}
	return n, nil
}

// ListUsers paginates over non-deleted users in descending creation order.
// The cursor encodes (created_at, id) so the next page picks up where the
// previous left off without missing or duplicating rows even under concurrent
// inserts.
func (r *UserRepo) ListUsers(
	ctx context.Context,
	page port.UserListPage,
) ([]domain.User, string, error) {
	size := page.PageSize
	if size <= 0 {
		size = 50
	}
	if size > 200 {
		size = 200
	}

	args := []any{size}
	where := `status != 'deleted'`

	if page.Cursor != "" {
		cursorTS, cursorID, err := decodeUserCursor(page.Cursor)
		if err != nil {
			return nil, "", fmt.Errorf("list users: %w", err)
		}
		args = append(args, cursorTS, cursorID)
		where += ` AND (created_at, id) < ($2, $3)`
	}

	q := `SELECT ` + userColumns + `
	         FROM users
	         WHERE ` + where + `
	         ORDER BY created_at DESC, id DESC
	         LIMIT $1`

	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	out := make([]domain.User, 0, size)
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list users: %w", err)
	}

	var nextCursor string
	if len(out) == size {
		last := out[len(out)-1]
		nextCursor = encodeUserCursor(last.CreatedAt, last.ID)
	}
	return out, nextCursor, nil
}

// ---------------------------------------------------------------------------
// Scan helpers
// ---------------------------------------------------------------------------

func scanUserRow(row rowScanner) (domain.User, error) {
	var (
		id                     uuid.UUID
		username, email        string
		emailVerified          bool
		displayName            string
		avatarURL              *string
		locale, timezone, unit string
		dateFormat, timeFormat string
		role, status           string
		statusReason           *string
		mustChange             bool
		createdAt, updatedAt   time.Time
		lastLoginAt            *time.Time
		profileIsPublic        bool
		secondFactor           bool
	)
	err := row.Scan(
		&id, &username, &email, &emailVerified, &displayName, &avatarURL,
		&locale, &timezone, &unit,
		&dateFormat, &timeFormat,
		&role, &status, &statusReason,
		&mustChange,
		&createdAt, &updatedAt, &lastLoginAt,
		&profileIsPublic, &secondFactor,
	)
	if err != nil {
		return domain.User{}, err
	}

	u := domain.User{
		ID:                  domain.UserID(id),
		Username:            username,
		Email:               email,
		EmailVerified:       emailVerified,
		DisplayName:         displayName,
		Locale:              locale,
		Timezone:            timezone,
		Units:               domain.UserUnits(unit),
		DateFormat:          dateFormat,
		TimeFormat:          timeFormat,
		Role:                domain.UserRole(role),
		Status:              domain.UserStatus(status),
		MustChangePassword:  mustChange,
		ProfileIsPublic:     profileIsPublic,
		PasskeySecondFactor: secondFactor,
		CreatedAt:           createdAt,
		UpdatedAt:           updatedAt,
		LastLoginAt:         lastLoginAt,
	}
	if avatarURL != nil {
		u.AvatarURL = *avatarURL
	}
	if statusReason != nil {
		u.StatusReason = *statusReason
	}
	return u, nil
}

func scanUserRowWithCredentials(row rowScanner) (domain.User, string, error) {
	// We re-implement Scan to add the trailing password_hash field rather
	// than calling scanUserRow + a second query: this keeps the
	// constant-time uniformity (one query, one row).
	var (
		id                     uuid.UUID
		username, email        string
		emailVerified          bool
		displayName            string
		avatarURL              *string
		locale, timezone, unit string
		dateFormat, timeFormat string
		role, status           string
		statusReason           *string
		mustChange             bool
		createdAt, updatedAt   time.Time
		lastLoginAt            *time.Time
		profileIsPublic        bool
		secondFactor           bool
		passwordHash           string
	)
	err := row.Scan(
		&id, &username, &email, &emailVerified, &displayName, &avatarURL,
		&locale, &timezone, &unit,
		&dateFormat, &timeFormat,
		&role, &status, &statusReason,
		&mustChange,
		&createdAt, &updatedAt, &lastLoginAt,
		&profileIsPublic, &secondFactor,
		&passwordHash,
	)
	if err != nil {
		return domain.User{}, "", err
	}
	u := domain.User{
		ID:                  domain.UserID(id),
		Username:            username,
		Email:               email,
		EmailVerified:       emailVerified,
		DisplayName:         displayName,
		Locale:              locale,
		Timezone:            timezone,
		Units:               domain.UserUnits(unit),
		DateFormat:          dateFormat,
		TimeFormat:          timeFormat,
		Role:                domain.UserRole(role),
		Status:              domain.UserStatus(status),
		MustChangePassword:  mustChange,
		ProfileIsPublic:     profileIsPublic,
		PasskeySecondFactor: secondFactor,
		CreatedAt:           createdAt,
		UpdatedAt:           updatedAt,
		LastLoginAt:         lastLoginAt,
	}
	if avatarURL != nil {
		u.AvatarURL = *avatarURL
	}
	if statusReason != nil {
		u.StatusReason = *statusReason
	}
	return u, passwordHash, nil
}

// wrapNotFound translates pgx.ErrNoRows into domain.ErrNotFound while
// preserving any other error as-is.
func wrapNotFound(u domain.User, err error, ctx string) (domain.User, error) {
	if err == nil {
		return u, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, fmt.Errorf("%s: %w", ctx, domain.ErrNotFound)
	}
	return domain.User{}, fmt.Errorf("%s: %w", ctx, err)
}

// ---------------------------------------------------------------------------
// Unique-violation translation
// ---------------------------------------------------------------------------

func mapUserUniqueViolation(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return fmt.Errorf("create user: %w", err)
	}
	switch pgErr.ConstraintName {
	case "users_username_key":
		return fmt.Errorf("create user: %w", port.ErrUsernameTaken)
	case "users_email_key":
		return fmt.Errorf("create user: %w", port.ErrEmailTaken)
	}
	return fmt.Errorf("create user: %w", err)
}

// ---------------------------------------------------------------------------
// Cursor codec for ListUsers keyset pagination
// ---------------------------------------------------------------------------

type userCursorPayload struct {
	TS time.Time `json:"ts"`
	ID string    `json:"id"`
}

func encodeUserCursor(ts time.Time, id domain.UserID) string {
	b, _ := json.Marshal(userCursorPayload{TS: ts, ID: id.String()})
	return base64.URLEncoding.EncodeToString(b)
}

func decodeUserCursor(c string) (time.Time, uuid.UUID, error) {
	b, err := base64.URLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("decode cursor: %w", err)
	}
	var p userCursorPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("unmarshal cursor: %w", err)
	}
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("parse cursor id: %w", err)
	}
	return p.TS, id, nil
}
