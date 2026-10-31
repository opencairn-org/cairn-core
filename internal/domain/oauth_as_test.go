package domain

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func TestVerifyPKCE(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	if !VerifyPKCE(verifier, challenge, "S256") {
		t.Fatal("valid S256 verifier should pass")
	}
	if !VerifyPKCE(verifier, challenge, "") {
		t.Fatal("empty method should default to S256 and pass")
	}
	if VerifyPKCE("wrong", challenge, "S256") {
		t.Fatal("wrong verifier must fail")
	}
	if VerifyPKCE(verifier, challenge, "plain") {
		t.Fatal("plain method must be rejected (OAuth 2.1)")
	}
	if VerifyPKCE("", challenge, "S256") || VerifyPKCE(verifier, "", "S256") {
		t.Fatal("empty verifier/challenge must fail")
	}
}

func TestFilterRequestedScopes(t *testing.T) {
	clientAllowed := []string{ScopeActivitiesRead, ScopeProfileRead, ScopeActivitiesWrite}

	// Requested subset is intersected with allowed + catalog, catalog-ordered.
	got := FilterRequestedScopes([]string{ScopeProfileRead, ScopeActivitiesRead}, clientAllowed)
	want := []string{ScopeActivitiesRead, ScopeProfileRead}
	if SortedScopeString(got) != SortedScopeString(want) {
		t.Fatalf("got %v want %v", got, want)
	}

	// A requested scope the client may not have is dropped.
	got = FilterRequestedScopes([]string{ScopeSocialWrite, ScopeActivitiesRead}, clientAllowed)
	if ScopesContain(JoinScopes(got), ScopeSocialWrite) {
		t.Fatalf("disallowed scope leaked: %v", got)
	}

	// Empty request defaults to the client's allowed scopes.
	got = FilterRequestedScopes(nil, clientAllowed)
	if len(got) != 3 {
		t.Fatalf("empty request should default to all client scopes, got %v", got)
	}
}

func TestScopeHelpers(t *testing.T) {
	if !IsWriteScope(ScopeActivitiesWrite) || IsWriteScope(ScopeActivitiesRead) {
		t.Fatal("IsWriteScope misclassified")
	}
	if !ScopesHaveAnyWrite("activities:read social:write") {
		t.Fatal("should detect a write scope")
	}
	if ScopesHaveAnyWrite("activities:read profile:read") {
		t.Fatal("should not detect a write scope among read-only")
	}
	if !ScopesContain("a:read b:read", "b:read") || ScopesContain("a:read", "b:read") {
		t.Fatal("ScopesContain wrong")
	}
}

func TestOAuthClientHelpers(t *testing.T) {
	pub := OAuthClient{ClientID: "x", RedirectURIs: []string{"https://app/cb"}, GrantTypes: []string{"authorization_code"}}
	if !pub.IsPublic() {
		t.Fatal("no secret => public")
	}
	if !pub.AllowsRedirectURI("https://app/cb") || pub.AllowsRedirectURI("https://evil/cb") {
		t.Fatal("redirect matching wrong")
	}
	if got, ok := pub.RegisteredRedirectURI("https://app/cb"); !ok || got != "https://app/cb" {
		t.Fatalf("RegisteredRedirectURI = %q, %v", got, ok)
	}
	for _, bad := range []string{"", "https://app/cb/", "https://APP/cb", "https://app/cb?x=1"} {
		if _, ok := pub.RegisteredRedirectURI(bad); ok {
			t.Fatalf("RegisteredRedirectURI(%q) matched", bad)
		}
	}
	if _, ok := (OAuthClient{RedirectURIs: []string{""}}).RegisteredRedirectURI(""); ok {
		t.Fatal("empty registered redirect must not match")
	}
	if !pub.AllowsGrant("authorization_code") || pub.AllowsGrant("password") {
		t.Fatal("grant matching wrong")
	}

	secret := "s3cr3t"
	h := sha256.Sum256([]byte(secret))
	conf := OAuthClient{ClientID: "y", SecretHash: h[:]}
	if conf.IsPublic() {
		t.Fatal("with secret => confidential")
	}
	if !conf.VerifySecret(secret) || conf.VerifySecret("nope") {
		t.Fatal("secret verification wrong")
	}
}

func TestOAuthClientLoopbackRedirects(t *testing.T) {
	c := OAuthClient{RedirectURIs: []string{"http://127.0.0.1:0/oauth/callback", "http://[::1]/cb", "http://localhost:9000/cb", "org.opencairn.cairn:/oauth/callback"}}
	// Any port matches a registered loopback redirect; the target is the registration on that port.
	for _, uri := range []string{"http://127.0.0.1:51234/oauth/callback", "http://127.0.0.1/oauth/callback", "http://[::1]:7777/cb"} {
		got, ok := c.RegisteredRedirectURI(uri)
		if !ok || got != uri {
			t.Errorf("RegisteredRedirectURI(%q) = %q, %v; want the requested uri", uri, got, ok)
		}
	}
	// Path, query, scheme, host literal and user info must still match.
	for _, uri := range []string{"http://127.0.0.1:1/other", "http://127.0.0.1:1/oauth/callback?x=1", "https://127.0.0.1:1/oauth/callback",
		"http://localhost:9001/cb", "http://127.0.0.2:1/oauth/callback", "http://u@127.0.0.1:1/oauth/callback", "http://127.0.0.1:1/oauth/callback#f"} {
		if _, ok := c.RegisteredRedirectURI(uri); ok {
			t.Errorf("RegisteredRedirectURI(%q) matched", uri)
		}
	}
	// Exact matches still return the stored value; a loopback registration
	// does not loosen custom schemes or localhost.
	if got, ok := c.RegisteredRedirectURI("org.opencairn.cairn:/oauth/callback"); !ok || got != "org.opencairn.cairn:/oauth/callback" {
		t.Fatalf("custom scheme: %q %v", got, ok)
	}
	if got, ok := c.RegisteredRedirectURI("http://localhost:9000/cb"); !ok || got != "http://localhost:9000/cb" {
		t.Fatalf("localhost exact: %q %v", got, ok)
	}
}

func TestScopesGrant(t *testing.T) {
	ro := []string{ScopeActivitiesRead, ScopeGearRead}
	if !ScopesGrant(ro, "activities", false) || ScopesGrant(ro, "activities", true) {
		t.Fatal("read scope must grant reads only")
	}
	rw := []string{ScopeActivitiesWrite}
	if !ScopesGrant(rw, "activities", false) || !ScopesGrant(rw, "activities", true) {
		t.Fatal("write scope must grant reads and writes")
	}
	if ScopesGrant(ro, "notifications", false) {
		t.Fatal("foreign family must not be granted")
	}
	if ScopeFamily("gear:write") != "gear" || ScopeFamily("x") != "" {
		t.Fatal("ScopeFamily wrong")
	}
	for _, s := range SupportedScopes {
		if ScopeFamily(s) == "" || (!IsWriteScope(s) && !strings.HasSuffix(s, ":read")) {
			t.Errorf("scope %q is not family:read|write", s)
		}
	}
}
