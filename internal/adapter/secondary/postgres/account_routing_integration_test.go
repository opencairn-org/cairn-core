//go:build integration

package postgres

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// One Strava app has one push subscription for all its athletes: routing must
// match the owner too, and never pick between users.
func TestExternalAccounts_SharedSubscriptionRouting(t *testing.T) {
	ctx, pool := requirePool(t)
	accts := NewExternalAccountRepo(pool, 0, 0)
	alice, bob := seedUser(t), seedUser(t)
	sfx := alice.String()[:8]
	sub, athA, athB := "sub-"+sfx, "a-"+sfx, "b-"+sfx
	conn := func(user domain.UserID, clientID string) *domain.ConnectionID {
		t.Helper()
		id := domain.ConnectionID(uuid.New())
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_provider_configs (id, user_id, provider, client_id) VALUES ($1, $2, 'strava', $3)`,
			id.UUID(), user.UUID(), clientID); err != nil {
			t.Fatal(err)
		}
		return &id
	}
	create := func(user domain.UserID, athlete string, c *domain.ConnectionID) domain.ExternalAccountID {
		t.Helper()
		id, err := accts.CreateAccount(ctx, domain.ExternalAccount{UserID: user, Provider: "strava", ProviderAccountID: athlete, ConnectionID: c, AssignedWorkerName: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if err := accts.RegisterWebhookSubscription(ctx, "strava", sub, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	app := "app-" + sfx
	a := create(alice, athA, conn(alice, app))
	b := create(bob, athB, conn(bob, app))

	for owner, want := range map[string]domain.ExternalAccountID{athA: a, athB: b} {
		got, err := accts.FindBySubscription(ctx, "strava", sub, owner, "main")
		if err != nil || got.ID != want {
			t.Errorf("owner %s = %v/%v, want %s", owner, got.ID, err, want)
		}
	}
	if _, err := accts.FindBySubscription(ctx, "strava", sub, "", "main"); !errors.Is(err, domain.ErrAmbiguous) {
		t.Errorf("subscription-only: %v", err)
	}
	if _, err := accts.FindBySubscription(ctx, "strava", sub, "nobody", "main"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown owner: %v", err)
	}

	// Link-time guard: bob already holds athB, alice does not.
	if held, err := accts.HeldByOtherUser(ctx, "strava", athB, alice); err != nil || !held {
		t.Errorf("held by bob: %v/%v", held, err)
	}
	if held, err := accts.HeldByOtherUser(ctx, "strava", athB, bob); err != nil || held {
		t.Errorf("own account: %v/%v", held, err)
	}
	if held, err := accts.HeldByOtherUser(ctx, "strava", "nobody-"+sfx, alice); err != nil || held {
		t.Errorf("unlinked athlete: %v/%v", held, err)
	}

	// Pre-existing duplicate (both users hold athB): ambiguous, not arbitrary.
	create(alice, athB, conn(alice, app))
	if _, err := accts.FindBySubscription(ctx, "strava", sub, athB, "main"); !errors.Is(err, domain.ErrAmbiguous) {
		t.Errorf("duplicate by subscription: %v", err)
	}
	if _, err := accts.FindByProviderAndExternalID(ctx, "strava", athB, "", "main"); !errors.Is(err, domain.ErrAmbiguous) {
		t.Errorf("duplicate by owner: %v", err)
	}
	// A new subscription prefers the holder not bound to another one.
	c := create(seedUser(t), "c-"+sfx, nil)
	if err := accts.RegisterWebhookSubscription(ctx, "strava", "other-"+sub, c); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE external_accounts SET provider_account_id = $1 WHERE id = $2`, athA, c.UUID()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE external_accounts SET webhook_subscription_id = NULL WHERE id = $1`, a.UUID()); err != nil {
		t.Fatal(err)
	}
	got, err := accts.FindByProviderAndExternalID(ctx, "strava", athA, "new-"+sub, "main")
	if err != nil || got.ID != a {
		t.Errorf("unbound preference = %v/%v, want %s", got.ID, err, a)
	}
	if _, err := accts.FindByProviderAndExternalID(ctx, "strava", athA, "", "main"); !errors.Is(err, domain.ErrAmbiguous) {
		t.Errorf("no subscription, two holders: %v", err)
	}
}
