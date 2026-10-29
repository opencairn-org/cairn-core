//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestWebhookTokenRepo(t *testing.T) {
	ctx, pool := requirePool(t)
	userID := seedUser(t)
	accts := NewExternalAccountRepo(pool, 0, 0)
	provider := "wh-" + userID.String()[:8]
	var ids []domain.ExternalAccountID
	for _, pa := range []string{"a", "b"} {
		id, err := accts.CreateAccount(ctx, domain.ExternalAccount{UserID: userID, Provider: provider, ProviderAccountID: pa})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows, total, err := accts.ListWebhookTokens(ctx, provider, 10, 0)
	if err != nil || total != 2 || len(rows) != 2 || rows[0].RotatedAt != nil || rows[0].Provider != provider {
		t.Fatalf("list: %v %d %+v", err, total, rows)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, hash := domain.NewWebhookSigningToken()
	if err := accts.SetWebhookSigningToken(ctx, ids[0], hash, now); err != nil {
		t.Fatal(err)
	}
	if err := accts.SetWebhookSigningToken(ctx, domain.ExternalAccountID{}, hash, now); err == nil {
		t.Fatal("unknown account must fail")
	}
	// A never-rotated account is older than any cut-off; a just-rotated one is not.
	cut := now.Add(-time.Minute)
	sel, err := accts.SelectAccountsForRotation(ctx, provider, &cut, nil)
	if err != nil || len(sel) != 1 || sel[0] != ids[1] {
		t.Fatalf("stale selection: %v %v", err, sel)
	}
	sel, err = accts.SelectAccountsForRotation(ctx, "", nil, ids[:1])
	if err != nil || len(sel) != 1 || sel[0] != ids[0] {
		t.Fatalf("explicit selection: %v %v", err, sel)
	}
	rows, _, _ = accts.ListWebhookTokens(ctx, provider, 1, 1)
	if len(rows) != 1 {
		t.Fatalf("page: %+v", rows)
	}
}
