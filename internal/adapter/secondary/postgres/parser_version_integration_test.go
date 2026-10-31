//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestMarkSourcesOutOfDate_ParserVersions(t *testing.T) {
	ctx, pool := requirePool(t)
	userID := seedUser(t)
	activities := NewActivityRepo(pool)
	pkg := "pkg-" + uuid.NewString()[:8] // the update is global per package

	saved := map[string]domain.SourceID{}
	for _, v := range []string{"1", "1.2", "1.3", "2.0", "dev"} {
		aid := seedGearActivity(t, ctx, activities, userID, nil, 1000, 100)
		src := domain.ActivitySource{
			ID: domain.SourceID(uuid.New()), ActivityID: aid, UserID: userID, Provider: "strava",
			ExternalID: "pv-" + uuid.NewString()[:8], SourceWorkerPackage: pkg, SourceWorkerVersion: v,
			SourceWorkerRelease: "v0.5.0",
			Parsed: domain.ActivitySourcePayload{
				Type: domain.ActivityTypeRide, StartTime: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC),
				Timezone: "UTC", ElapsedDuration: time.Hour,
			},
			Status: domain.SourceStatusActive, ReimportStatus: domain.ReimportStatusCurrent, ImportedAt: time.Now(),
		}
		if err := activities.SaveSource(ctx, src); err != nil {
			t.Fatalf("SaveSource %s: %v", v, err)
		}
		saved[v] = src.ID
	}

	check := func(live domain.ParserVersion, wantN int, want map[string]string) {
		t.Helper()
		n, err := activities.MarkSourcesOutOfDate(ctx, "strava", pkg, live)
		if err != nil || n != wantN {
			t.Fatalf("%v: updated %d/%v, want %d", live, n, err, wantN)
		}
		for v, reason := range want {
			got, err := activities.GetSource(ctx, saved[v])
			if err != nil {
				t.Fatal(err)
			}
			if got.ReimportStatusReason != reason || (reason != "") != (got.ReimportStatus == domain.ReimportStatusUpdateAvailable) {
				t.Errorf("%v: source %s = %s/%q, want %q", live, v, got.ReimportStatus, got.ReimportStatusReason, reason)
			}
			// The SQL classification must agree with the domain rule.
			if stored, err := domain.ParseParserVersion(v); err == nil && reason != "" && live.UpgradeFor(stored).Reason() != reason {
				t.Errorf("%v: SQL reason %q for %s disagrees with domain %q", live, reason, v, live.UpgradeFor(stored).Reason())
			}
			if got.SourceWorkerRelease != "v0.5.0" {
				t.Errorf("release not round-tripped: %q", got.SourceWorkerRelease)
			}
		}
	}
	check(domain.ParserVersion{Dataset: 1, Parser: 3}, 2, map[string]string{"1": "newer_parser", "1.2": "newer_parser", "1.3": "", "2.0": "", "dev": ""})
	check(domain.ParserVersion{Dataset: 1, Parser: 3}, 0, nil) // idempotent
	check(domain.ParserVersion{Dataset: 2, Parser: 0}, 3, map[string]string{"1": "newer_dataset", "1.2": "newer_dataset", "1.3": "newer_dataset", "2.0": "", "dev": ""})
}
