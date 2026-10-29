//go:build integration

package main

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/opencairn-org/cairn-core/internal/domain"
	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
)

// TestAPI_LoginGuard: repeated password failures lock the identifier, the
// lock is audited and the correct password is refused while it lasts.
func TestAPI_LoginGuard(t *testing.T) {
	env := newITestEnv(t)
	alice := env.user("alice", domain.UserRoleUser)
	attempt := func(password string) string {
		resp, err := env.anon().http.PostForm(env.srv.URL+"/auth/password", url.Values{"identifier": {"alice"}, "password": {password}})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("status %d", resp.StatusCode)
		}
		return resp.Header.Get("Location")
	}
	for i := range authuc.DefaultMaxFailuresPerUser - 1 {
		if loc := attempt("wrong"); loc != "/login?error=invalid_credentials" {
			t.Fatalf("attempt %d: %s", i, loc)
		}
	}
	if loc := attempt("wrong"); loc != "/login?error=invalid_credentials" {
		t.Fatalf("locking attempt: %s", loc)
	}
	if loc := attempt("correct horse battery"); loc != "/login?error=too_many_attempts" {
		t.Fatalf("locked: %s", loc)
	}
	entries, err := env.app.AuditLog.List(context.Background(), domain.AuditFilter{Actions: []string{domain.AuditLoginFailed}})
	if err != nil || len(entries) != authuc.DefaultMaxFailuresPerUser+1 {
		t.Fatalf("audit: %v %d", err, len(entries))
	}
	var warned bool
	for _, e := range entries {
		warned = warned || e.Severity == domain.AuditWarn && e.ResourceID == "id:alice"
	}
	if !warned {
		t.Fatalf("no lock entry: %+v", entries)
	}
	_ = alice
}
