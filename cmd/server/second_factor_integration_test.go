//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// TestAPI_SecondFactor walks the passkey-after-password flow on REST and the
// Complete2FA RPC. The WebAuthn assertion itself cannot run without a browser
// authenticator, so the challenge is satisfied through the use case.
func TestAPI_SecondFactor(t *testing.T) {
	env := newITestEnv(t)
	alice := env.user("alice2fa", domain.UserRoleUser)
	ctx := context.Background()

	// Needs a passkey first.
	alice.do(http.MethodPut, "/api/passkeys/second-factor", map[string]any{"enabled": true}, http.StatusBadRequest)
	if _, err := env.app.Passkeys.Create(ctx, domain.Passkey{UserID: alice.userID, CredentialID: []byte("cred-" + alice.name), CredentialJSON: []byte(`{}`), Name: "test key"}); err != nil {
		t.Fatal(err)
	}
	alice.do(http.MethodPut, "/api/passkeys/second-factor", map[string]any{"enabled": true}, http.StatusOK)
	var keys struct {
		SecondFactor bool `json:"second_factor"`
	}
	_ = json.Unmarshal(alice.do(http.MethodGet, "/api/passkeys", nil, http.StatusOK), &keys)
	if !keys.SecondFactor {
		t.Fatal("second factor flag not reported")
	}

	// A password sign-in now yields a challenge instead of a session.
	fresh := env.anon()
	resp, err := fresh.http.PostForm(env.srv.URL+"/auth/password", url.Values{
		"identifier": {alice.name}, "password": {"correct horse battery"}, "redirect": {"/activities"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/login?challenge=") {
		t.Fatalf("password login with 2FA: %d %s", resp.StatusCode, loc)
	}
	u, _ := url.Parse(loc)
	token := u.Query().Get("challenge")
	fresh.do(http.MethodGet, "/api/overview", nil, http.StatusUnauthorized)
	fresh.do(http.MethodPost, "/auth/2fa/complete", map[string]any{"challenge_token": token}, http.StatusBadRequest)
	if err := env.app.SecondFactor.Satisfy(ctx, token, alice.userID); err != nil {
		t.Fatal(err)
	}
	fresh.do(http.MethodPost, "/auth/2fa/complete", map[string]any{"challenge_token": token}, http.StatusOK)
	fresh.do(http.MethodGet, "/api/overview", nil, http.StatusOK)
	fresh.do(http.MethodPost, "/auth/2fa/complete", map[string]any{"challenge_token": token}, http.StatusBadRequest)

	// Complete2FA on Connect hands a bearer session token that both surfaces accept.
	token, err = env.app.SecondFactor.Begin(ctx, alice.userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.app.SecondFactor.Satisfy(ctx, token, alice.userID); err != nil {
		t.Fatal(err)
	}
	native := env.anon()
	res := native.post("/cairn.v1.AuthService/Complete2FA", map[string]any{"challengeToken": token}, http.StatusOK)
	sess, _ := res["session"].(map[string]any)
	bearer, _ := sess["sessionToken"].(string)
	if !strings.HasPrefix(bearer, domain.SessionTokenPrefix) {
		t.Fatalf("Complete2FA session = %v", res)
	}
	native.bearer = bearer
	native.do(http.MethodGet, "/api/overview", nil, http.StatusOK)
	native.rpc("/cairn.v1.AuthService/GetCurrentUser", nil, http.StatusOK)
	native.rpc("/cairn.v1.AuthService/ListSessions", nil, http.StatusOK)

	alice.do(http.MethodPut, "/api/passkeys/second-factor", map[string]any{"enabled": false}, http.StatusOK)
	alice.rpc("/cairn.v1.AuthService/SetPasskeySecondFactor", map[string]any{"enabled": true}, http.StatusOK)
}
