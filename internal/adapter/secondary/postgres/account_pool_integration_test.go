//go:build integration

package postgres

import (
	"errors"
	"testing"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestExternalAccounts_OneConnectionPerPool(t *testing.T) {
	ctx, pool := requirePool(t)
	userID := seedUser(t)
	accts := NewExternalAccountRepo(pool, 0, 0)
	gear := NewGearRepo(pool)
	pa := "pool-" + userID.String()[:8]
	create := func(worker string) domain.ExternalAccountID {
		t.Helper()
		id, err := accts.CreateAccount(ctx, domain.ExternalAccount{UserID: userID, Provider: "garmin", ProviderAccountID: pa, AssignedWorkerName: worker})
		if err != nil {
			t.Fatalf("CreateAccount(%q): %v", worker, err)
		}
		return id
	}

	legacy := create("")        // pre-0.5 row, no pool
	api := create("garmin-api") // adopts the legacy row
	if api != legacy {
		t.Fatalf("legacy row not adopted: %s vs %s", api, legacy)
	}
	fit := create("garmin-fit")
	if fit == api {
		t.Fatal("second pool reused the first connection")
	}
	if again := create("garmin-fit"); again != fit {
		t.Fatalf("re-connect created a new row: %s vs %s", again, fit)
	}

	for worker, want := range map[string]domain.ExternalAccountID{"garmin-api": api, "garmin-fit": fit} {
		got, err := accts.FindByProviderAndExternalID(ctx, "garmin", pa, worker)
		if err != nil || got.ID != want {
			t.Errorf("lookup for %s = %v/%v, want %s", worker, got.ID, err, want)
		}
	}
	if _, err := accts.FindByProviderAndExternalID(ctx, "garmin", pa, ""); err != nil {
		t.Errorf("pool-less lookup: %v", err)
	}
	if err := accts.SetAssignedWorker(ctx, fit, "garmin-api"); !errors.Is(err, domain.ErrPoolTaken) {
		t.Errorf("moving onto a taken pool: %v", err)
	}

	// Gear refs of the two connections are siblings.
	if err := gear.UpsertExternalRef(ctx, domain.GearExternalRef{Provider: "garmin", ExternalAccountID: api, ExternalID: "g1"}); err != nil {
		t.Fatal(err)
	}
	sibs, err := gear.ListSiblingRefs(ctx, domain.GearRefKey{Provider: "garmin", ExternalAccountID: fit, ExternalID: "g1"})
	if err != nil || len(sibs) != 1 || sibs[0].ExternalAccountID != api {
		t.Fatalf("siblings = %+v / %v", sibs, err)
	}
}
