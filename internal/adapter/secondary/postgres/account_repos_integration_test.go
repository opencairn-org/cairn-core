//go:build integration

package postgres

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// TestAccountRepos covers the self-service additions: session listing,
// linked identity listing/deletion and the profile update.
func TestAccountRepos(t *testing.T) {
	ctx, pool := requirePool(t)
	users := NewUserRepo(pool)
	uid := domain.UserID(uuid.New())
	suffix := uid.String()[:8]
	if err := users.CreateUser(ctx, domain.User{
		ID: uid, Username: "acct-" + suffix, Email: "acct-" + suffix + "@example.com",
		DisplayName: "Acct", Role: domain.UserRoleUser, Status: domain.UserStatusActive, Units: domain.UserUnitsMetric,
	}, domain.Credentials{UserID: uid, PasswordHash: "x"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	t.Run("sessions", func(t *testing.T) {
		sessions := NewSessionRepo(pool)
		now := time.Now().UTC()
		mk := func(expires time.Time) domain.SessionID {
			hash := sha256.Sum256([]byte(uuid.NewString()))
			id, err := sessions.Create(ctx, domain.Session{
				UserID: uid, TokenHash: hash[:], AuthMethod: domain.SessionAuthPassword,
				CreatedAt: now, LastSeenAt: now, ExpiresAt: expires,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			return id
		}
		live := mk(now.Add(time.Hour))
		expired := mk(now.Add(-time.Hour))
		revoked := mk(now.Add(time.Hour))
		if err := sessions.Revoke(ctx, revoked); err != nil {
			t.Fatal(err)
		}
		list, err := sessions.ListForUser(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].ID != live || list[0].TokenHash != nil {
			t.Fatalf("ListForUser = %+v (expired %s, revoked %s)", list, expired, revoked)
		}
	})

	t.Run("identities", func(t *testing.T) {
		idents := NewLinkedIdentityRepo(pool)
		id, err := idents.Create(ctx, domain.LinkedIdentity{UserID: uid, Provider: "google", Subject: "sub-" + suffix, Email: "g@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		list, err := idents.ListForUser(ctx, uid)
		if err != nil || len(list) != 1 || list[0].ID != id || list[0].Email != "g@example.com" {
			t.Fatalf("ListForUser: %v %+v", err, list)
		}
		other := domain.UserID(uuid.New())
		if err := idents.Delete(ctx, other, id); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("foreign delete: %v", err)
		}
		if err := idents.Delete(ctx, uid, id); err != nil {
			t.Fatal(err)
		}
		if list, _ := idents.ListForUser(ctx, uid); len(list) != 0 {
			t.Fatalf("still linked: %+v", list)
		}
	})

	t.Run("profile", func(t *testing.T) {
		err := users.UpdateProfile(ctx, uid, port.ProfileUpdate{
			DisplayName: "Renamed", AvatarURL: "", Locale: "de-DE", Timezone: "Europe/Berlin", Units: "imperial",
		})
		if err != nil {
			t.Fatal(err)
		}
		u, err := users.GetUser(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		if u.DisplayName != "Renamed" || u.AvatarURL != "" || u.Locale != "de-DE" || u.Timezone != "Europe/Berlin" || u.Units != domain.UserUnitsImperial {
			t.Fatalf("profile %+v", u)
		}
		if err := users.UpdateProfile(ctx, domain.UserID(uuid.New()), port.ProfileUpdate{DisplayName: "x", Units: "metric"}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("unknown user: %v", err)
		}
	})
}
