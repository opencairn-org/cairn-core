//go:build integration

package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// TestAPI_AccountLifecycle covers account removal and webhook token rotation
// on REST and the ExternalAccountService RPCs, including ownership.
func TestAPI_AccountLifecycle(t *testing.T) {
	env := newITestEnv(t)
	alice := env.user("alice", domain.UserRoleUser)
	bob := env.user("bob", domain.UserRoleUser)
	ctx := context.Background()
	mk := func(pa string) domain.ExternalAccountID {
		id, err := env.app.ExternalAccounts.CreateAccount(ctx, domain.ExternalAccount{UserID: alice.userID, Provider: "strava", ProviderAccountID: pa, Status: domain.ExternalAccountStatusActive})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	rest, rpc := mk("rest"), mk("rpc")

	bob.do(http.MethodPost, "/api/accounts/"+rest.String()+"/webhook-token/rotate", nil, http.StatusNotFound)
	bob.do(http.MethodDelete, "/api/accounts/"+rest.String(), nil, http.StatusNotFound)
	alice.post("/api/accounts/"+rest.String()+"/webhook-token/rotate", nil, http.StatusOK)
	if before, _, _ := env.app.ExternalAccounts.ListWebhookTokens(ctx, "strava", 10, 0); len(before) != 2 || before[0].RotatedAt == nil && before[1].RotatedAt == nil {
		t.Fatalf("tokens: %+v", before)
	}
	got := alice.do(http.MethodDelete, "/api/accounts/"+rest.String()+"?delete_exclusive_activities=true", nil, http.StatusOK)
	if string(got) == "" {
		t.Fatal("empty delete response")
	}
	alice.do(http.MethodDelete, "/api/accounts/"+rest.String(), nil, http.StatusNotFound)
	alice.do(http.MethodPost, "/api/accounts/"+rest.String()+"/webhook-token/rotate", nil, http.StatusNotFound)

	bob.rpc("/cairn.v1.ExternalAccountService/RotateWebhookToken", map[string]any{"id": rpc.String()}, http.StatusNotFound)
	alice.rpc("/cairn.v1.ExternalAccountService/RotateWebhookToken", map[string]any{"id": rpc.String()}, http.StatusOK)
	bob.rpc("/cairn.v1.ExternalAccountService/DeleteExternalAccount", map[string]any{"id": rpc.String()}, http.StatusNotFound)
	alice.rpc("/cairn.v1.ExternalAccountService/DeleteExternalAccount", map[string]any{"id": rpc.String()}, http.StatusOK)
	alice.rpc("/cairn.v1.ExternalAccountService/DeleteExternalAccount", map[string]any{"id": rpc.String()}, http.StatusNotFound)
	if left, _ := env.app.ExternalAccounts.ListAccountsForUser(ctx, alice.userID); len(left) != 0 {
		t.Fatalf("accounts left: %+v", left)
	}
	if entries, err := env.app.AuditLog.List(ctx, domain.AuditFilter{}); err != nil || len(entries) < 4 {
		t.Fatalf("audit trail: %v %d", err, len(entries))
	}
}
