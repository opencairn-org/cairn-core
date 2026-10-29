//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestAuditRepoAndUserAdminColumns(t *testing.T) {
	ctx, pool := requirePool(t)
	users := NewUserRepo(pool)
	uid := domain.UserID(uuid.New())
	suffix := uid.String()[:8]
	if err := users.CreateUser(ctx, domain.User{
		ID: uid, Username: "aud-" + suffix, Email: "aud-" + suffix + "@example.com",
		DisplayName: "Aud", Role: domain.UserRoleUser, Status: domain.UserStatusActive, Units: domain.UserUnitsMetric,
	}, domain.Credentials{UserID: uid}); err != nil {
		t.Fatal(err)
	}

	repo := NewAuditRepo(pool)
	marker := "test-" + suffix
	if err := repo.Record(ctx, domain.AuditEntry{
		ActorUserID: &uid, ActorUsername: "aud-" + suffix, IPAddress: "10.0.0.7", UserAgent: "itest",
		Action: domain.AuditUserRoleChanged, Severity: domain.AuditHigh, ResourceType: "user", ResourceID: marker,
		Diff: []domain.FieldChange{{Field: "role", Before: "user", After: "admin"}}, Metadata: map[string]string{"reason": "promotion"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Record(ctx, domain.AuditEntry{Action: domain.AuditLoginFailed, Severity: domain.AuditWarn, ResourceType: "user", ResourceID: marker}); err != nil {
		t.Fatal(err)
	}
	list, err := repo.List(ctx, domain.AuditFilter{ResourceID: marker})
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	high, err := repo.List(ctx, domain.AuditFilter{ResourceID: marker, MinSeverity: domain.AuditHigh})
	if err != nil || len(high) != 1 || high[0].Action != domain.AuditUserRoleChanged || high[0].IPAddress != "10.0.0.7" || len(high[0].Diff) != 1 || high[0].Metadata["reason"] != "promotion" || high[0].ActorUserID == nil {
		t.Fatalf("high: %v %+v", err, high)
	}
	if byActor, _ := repo.List(ctx, domain.AuditFilter{ActorUserID: &uid}); len(byActor) != 1 {
		t.Fatalf("by actor: %d", len(byActor))
	}
	if byQ, _ := repo.List(ctx, domain.AuditFilter{ResourceID: marker, Query: "promotion"}); len(byQ) != 1 {
		t.Fatalf("by query: %d", len(byQ))
	}
	if none, _ := repo.List(ctx, domain.AuditFilter{ResourceID: marker, To: time.Now().Add(-time.Hour)}); len(none) != 0 {
		t.Fatalf("time bound: %d", len(none))
	}

	// New user columns.
	if err := users.UpdateEmail(ctx, uid, "aud2-"+suffix+"@example.com", false); err != nil {
		t.Fatal(err)
	}
	if err := users.SetMustChangePassword(ctx, uid, true); err != nil {
		t.Fatal(err)
	}
	u, _ := users.GetUser(ctx, uid)
	if u.Email != "aud2-"+suffix+"@example.com" || u.EmailVerified || !u.MustChangePassword {
		t.Fatalf("user %+v", u)
	}
	fps, err := NewActivityRepo(pool).UserFootprints(ctx, []domain.UserID{uid})
	if err != nil || fps[uid].Activities != 0 {
		t.Fatalf("footprints: %v %+v", err, fps)
	}
	if err := users.HardDeleteUser(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := users.GetUser(ctx, uid); err == nil {
		t.Fatal("user still present after hard delete")
	}
}
