//go:build integration

package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestAccountLifecycleRepo(t *testing.T) {
	ctx, pool := requirePool(t)
	uid := seedUser(t)
	accts := NewExternalAccountRepo(pool, 0, 0)
	acctID, err := accts.CreateAccount(ctx, domain.ExternalAccount{UserID: uid, Provider: "lc-" + uid.String()[:8], ProviderAccountID: "pa"})
	if err != nil {
		t.Fatal(err)
	}
	acts := NewActivityRepo(pool)
	now := time.Now().UTC()
	actID := domain.ActivityID(uuid.New())
	if err := acts.SaveActivity(ctx, domain.Activity{
		ID: actID, UserID: uid, Type: domain.ActivityTypeRide, Title: "t", StartTime: now, EndTime: now.Add(time.Hour),
		ElapsedDuration: time.Hour, MovingDuration: time.Hour, Timezone: "UTC", MergedAt: now, Privacy: domain.PrivacyPrivate,
	}); err != nil {
		t.Fatal(err)
	}
	payload := domain.ActivitySourcePayload{Type: domain.ActivityTypeRide, StartTime: now, EndTime: now.Add(time.Hour), ElapsedDuration: time.Hour, MovingDuration: time.Hour, Timezone: "UTC"}
	live, gone := domain.SourceID(uuid.New()), domain.SourceID(uuid.New())
	for _, s := range []struct {
		id     domain.SourceID
		status domain.SourceStatus
	}{{live, domain.SourceStatusActive}, {gone, domain.SourceStatusDetached}} {
		if err := acts.SaveSource(ctx, domain.ActivitySource{
			ID: s.id, ActivityID: actID, UserID: uid, Provider: "lc", ExternalAccountID: &acctID, ExternalID: s.id.String(),
			SourceWorkerName: "w", SourceWorkerVersion: "1.0", SourceWorkerPackage: "p", Status: s.status,
			ReimportStatus: domain.ReimportStatusCurrent, ImportedAt: now, UpdatedAt: now, Parsed: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO import_queue (external_account_id, user_id, provider, item_type, external_id) VALUES ($1, $2, 'lc', 'activity', 'q1')`, acctID.UUID(), uid.UUID()); err != nil {
		t.Fatal(err)
	}

	srcs, err := acts.ListSourcesForAccount(ctx, acctID)
	if err != nil || len(srcs) != 1 || srcs[0].ID != live {
		t.Fatalf("live sources: %v %+v", err, srcs)
	}
	if err := accts.DeleteAccount(ctx, acctID); err != nil {
		t.Fatal(err)
	}
	if err := accts.DeleteAccount(ctx, acctID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM import_queue WHERE user_id = $1`, uid.UUID()).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("queue cascade: %v %d", err, queued)
	}
	all, err := acts.ListAllSourcesForActivity(ctx, actID)
	if err != nil || len(all) != 2 || all[0].ExternalAccountID != nil || all[1].ExternalAccountID != nil {
		t.Fatalf("sources keep rows with the link nulled: %v %+v", err, all)
	}
}
