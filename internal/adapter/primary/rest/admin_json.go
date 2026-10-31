package rest

import (
	"net/http"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// adminGate writes 403 unless the caller is an admin; anonymous callers get 403 too.
func adminGate(a *Auth, w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	admin, ok := a.Admin(r)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
	}
	return admin, ok
}

// adminParseID parses the {id} path value into a typed ID or writes 400.
func adminParseID[T ~[16]byte](w http.ResponseWriter, r *http.Request) (T, bool) {
	id, err := domain.ParseUUID[T](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return id, false
	}
	return id, true
}

func adminTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// adminParseTime reads an RFC 3339 query value; zero when absent or malformed.
func adminParseTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// adminCSV splits a comma list, dropping blanks.
func adminCSV[T ~string](raw string) []T {
	var out []T
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, T(p))
		}
	}
	return out
}

func adminDeref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// adminUserJSON is the full user row of the admin views.
func adminUserJSON(u domain.User) map[string]any {
	return map[string]any{
		"id":                    u.ID.String(),
		"username":              u.Username,
		"email":                 u.Email,
		"email_verified":        u.EmailVerified,
		"display_name":          u.DisplayName,
		"avatar_url":            u.AvatarURL,
		"locale":                u.Locale,
		"timezone":              u.Timezone,
		"units":                 string(u.Units),
		"date_format":           u.DateFormat,
		"time_format":           u.TimeFormat,
		"role":                  string(u.Role),
		"status":                string(u.Status),
		"passkey_second_factor": u.PasskeySecondFactor,
		"created_at":            u.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":            u.UpdatedAt.UTC().Format(time.RFC3339),
		"last_login_at":         adminTimePtr(u.LastLoginAt),
	}
}
