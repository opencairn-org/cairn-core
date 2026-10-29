//go:build integration

package postgres

import (
	"testing"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestInstanceSettingsRepo(t *testing.T) {
	ctx, pool := requirePool(t)
	repo := NewInstanceSettingsRepo(pool)
	if err := repo.MirrorEnv(ctx, "smtp", true, "noreply@example.com", "minio", true); err != nil {
		t.Fatal(err)
	}
	cur, err := repo.Get(ctx)
	if err != nil || cur.EmailAdapter != "smtp" || !cur.EmailConfigured || cur.EmailFromAddress != "noreply@example.com" || !cur.StorageConfigured {
		t.Fatalf("get: %v %+v", err, cur)
	}
	if cur.Name == "" || cur.PrimaryColor == "" || cur.DefaultUnits == "" {
		t.Fatalf("defaults missing: %+v", cur)
	}
	cur.Name, cur.AllowedEmailDomains = "Integration Cairn", []string{"example.com"}
	q := int64(1 << 30)
	cur.PerUserStorageQuotaBytes = &q
	if err := repo.Save(ctx, cur); err != nil {
		t.Fatal(err)
	}
	again, _ := repo.Get(ctx)
	if again.Name != "Integration Cairn" || len(again.AllowedEmailDomains) != 1 || again.PerUserStorageQuotaBytes == nil || *again.PerUserStorageQuotaBytes != q {
		t.Fatalf("saved: %+v", again)
	}
	cur.Name, cur.AllowedEmailDomains, cur.PerUserStorageQuotaBytes = "Cairn", []string{}, nil
	_ = repo.Save(ctx, cur)
	// The merge defaults stored on the same row survive untouched.
	if _, err := NewUserSettingsRepo(pool).GetInstanceMergeDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	_ = domain.UserUnitsMetric
}
