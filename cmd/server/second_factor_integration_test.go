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

// TestAPI_MustChangePassword: an admin reset leaves a temporary password that
// cannot open a session; the sign-in completes only with a new password, and
// then runs the second factor when the account requires one.
func TestAPI_MustChangePassword(t *testing.T) {
	env := newITestEnv(t)
	admin := env.user("adminreset", domain.UserRoleAdmin)
	bob := env.user("bobreset", domain.UserRoleUser)
	ctx := context.Background()

	var reset struct {
		Temp string `json:"temporary_password"`
	}
	_ = json.Unmarshal(admin.do(http.MethodPost, "/api/admin/users/"+bob.userID.String()+"/reset-password", nil, http.StatusOK), &reset)
	if reset.Temp == "" {
		t.Fatal("no temporary password")
	}
	bob.do(http.MethodGet, "/api/overview", nil, http.StatusUnauthorized) // reset ended the session

	login := func(c *itestClient, password string) *http.Response {
		t.Helper()
		resp, err := c.http.PostForm(env.srv.URL+"/auth/password", url.Values{"identifier": {bob.name}, "password": {password}, "redirect": {"/activities"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}
	fresh := env.anon()
	resp := login(fresh, reset.Temp)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/login?set_password=") {
		t.Fatalf("login with temporary password: %d %s", resp.StatusCode, loc)
	}
	u, _ := url.Parse(loc)
	token := u.Query().Get("set_password")
	fresh.do(http.MethodGet, "/api/overview", nil, http.StatusUnauthorized)
	fresh.do(http.MethodPost, "/auth/password/required", map[string]any{"challenge_token": token, "password": "short"}, http.StatusBadRequest)
	fresh.do(http.MethodPost, "/auth/2fa/complete", map[string]any{"challenge_token": token}, http.StatusBadRequest)
	var out struct {
		LoggedIn  bool   `json:"logged_in"`
		Challenge string `json:"challenge"`
	}
	_ = json.Unmarshal(fresh.do(http.MethodPost, "/auth/password/required", map[string]any{"challenge_token": token, "password": "a brand new password"}, http.StatusOK), &out)
	if !out.LoggedIn || out.Challenge != "" {
		t.Fatalf("required: %+v", out)
	}
	fresh.do(http.MethodGet, "/api/overview", nil, http.StatusOK)
	fresh.do(http.MethodPost, "/auth/password/required", map[string]any{"challenge_token": token, "password": "a brand new password"}, http.StatusBadRequest)
	if resp := login(env.anon(), reset.Temp); resp.Header.Get("Location") != "/login?error=invalid_credentials" {
		t.Fatalf("temporary password still valid: %s", resp.Header.Get("Location"))
	}
	if resp := login(env.anon(), "a brand new password"); resp.Header.Get("Location") != "/activities" {
		t.Fatalf("new password: %s", resp.Header.Get("Location"))
	}

	// With the passkey second factor on, the new password hands over to the challenge.
	if _, err := env.app.Passkeys.Create(ctx, domain.Passkey{UserID: bob.userID, CredentialID: []byte("cred-" + bob.name), CredentialJSON: []byte(`{}`), Name: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := env.app.SecondFactor.SetEnabled(ctx, bob.userID, true); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(admin.do(http.MethodPost, "/api/admin/users/"+bob.userID.String()+"/reset-password", nil, http.StatusOK), &reset)
	fresh = env.anon()
	u, _ = url.Parse(login(fresh, reset.Temp).Header.Get("Location"))
	_ = json.Unmarshal(fresh.do(http.MethodPost, "/auth/password/required", map[string]any{"challenge_token": u.Query().Get("set_password"), "password": "yet another password"}, http.StatusOK), &out)
	if out.LoggedIn || out.Challenge == "" {
		t.Fatalf("required with 2fa: %+v", out)
	}
	fresh.do(http.MethodGet, "/api/overview", nil, http.StatusUnauthorized)
	if err := env.app.SecondFactor.Satisfy(ctx, out.Challenge, bob.userID); err != nil {
		t.Fatal(err)
	}
	fresh.do(http.MethodPost, "/auth/2fa/complete", map[string]any{"challenge_token": out.Challenge}, http.StatusOK)
	fresh.do(http.MethodGet, "/api/overview", nil, http.StatusOK)
}
