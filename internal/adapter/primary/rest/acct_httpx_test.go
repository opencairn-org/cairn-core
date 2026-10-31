package rest

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAcctSafeRedirect(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/",
		"/activities/1?tab=map":  "/activities/1?tab=map",
		"//evil.example":         "/",
		"/\\evil.example":        "/",
		"/a\\b":                  "/",
		"/\t/evil.example":       "/",
		"/x\r\nSet-Cookie: a=b":  "/",
		"https://evil.example/":  "/",
		"javascript:alert(1)":    "/",
		"/ok/../still-local?a=b": "/ok/../still-local?a=b",
	} {
		if got := acctSafeRedirect(in); got != want {
			t.Errorf("acctSafeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAcctSessionCookie(t *testing.T) {
	plain := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
	proxied := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
	proxied.Header.Set("X-Forwarded-Proto", "HTTPS")
	direct := httptest.NewRequest(http.MethodGet, "https://localhost/", nil)
	direct.TLS = &tls.ConnectionState{}
	if acctCookieSecure(plain, false) || !acctCookieSecure(proxied, false) || !acctCookieSecure(direct, false) {
		t.Error("dev mode: Secure must follow the request scheme")
	}
	if !acctCookieSecure(plain, true) {
		t.Error("https public URL: Secure must be set even on a plain-http request")
	}

	rec := httptest.NewRecorder()
	acctSetSessionCookie(rec, plain, true, "tok", time.Now().Add(time.Hour))
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].Name != SessionCookieName || !c[0].Secure || !c[0].HttpOnly || c[0].MaxAge <= 0 {
		t.Errorf("session cookie = %+v", c)
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer cairn_pat_x")
	if acctSessionToken(r) != "" {
		t.Error("a PAT is not a session token")
	}
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "cookie"})
	if acctSessionToken(r) != "cookie" {
		t.Error("the cookie wins")
	}
}

func TestLogoutRedirectsBrowsers(t *testing.T) {
	h := &OIDCHandlers{}
	for name, tc := range map[string]struct {
		header, value string
		want          int
	}{
		"form navigation": {"Sec-Fetch-Mode", "navigate", http.StatusSeeOther},
		"html accept":     {"Accept", "text/html,application/xhtml+xml", http.StatusSeeOther},
		"api client":      {"Accept", "application/json", http.StatusNoContent},
	} {
		r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
		r.Header.Set(tc.header, tc.value)
		w := httptest.NewRecorder()
		h.logout(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: status %d, want %d", name, w.Code, tc.want)
		}
		if tc.want == http.StatusSeeOther && w.Header().Get("Location") != "/login" {
			t.Errorf("%s: Location %q, want /login", name, w.Header().Get("Location"))
		}
		if c := w.Result().Cookies(); len(c) != 1 || c[0].Name != SessionCookieName || c[0].MaxAge >= 0 {
			t.Errorf("%s: session cookie not cleared: %v", name, c)
		}
	}
}
