package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSafeRedirect(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/",
		"/activities/1?tab=map":  "/activities/1?tab=map",
		"/":                      "/",
		"//evil.example":         "/",
		"/\\evil.example":        "/",
		"/a\\b":                  "/",
		"/\t/evil.example":       "/",
		"/x\r\nSet-Cookie: a=b":  "/",
		"https://evil.example/":  "/",
		"evil.example":           "/",
		"javascript:alert(1)":    "/",
		"/ok/../still-local?a=b": "/ok/../still-local?a=b",
	} {
		if got := safeRedirect(in); got != want {
			t.Errorf("safeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCookieSecure(t *testing.T) {
	t.Cleanup(func() { secureCookiesAlways = false })
	plain := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
	proxied := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
	proxied.Header.Set("X-Forwarded-Proto", "HTTPS")
	direct := httptest.NewRequest(http.MethodGet, "https://localhost/", nil)
	direct.TLS = &tls.ConnectionState{}

	secureCookiesAlways = false
	if cookieSecure(plain) || !cookieSecure(proxied) || !cookieSecure(direct) {
		t.Error("dev mode: Secure must follow the request scheme")
	}
	secureCookiesAlways = true
	if !cookieSecure(plain) {
		t.Error("https public URL: Secure must be set even on a plain-http request")
	}

	rec := httptest.NewRecorder()
	setSessionCookie(rec, plain, "tok", time.Now().Add(time.Hour))
	if c := rec.Result().Cookies(); len(c) != 1 || !c[0].Secure || !c[0].HttpOnly {
		t.Errorf("session cookie = %+v", c)
	}
}

func TestAppendQuery(t *testing.T) {
	got := appendQuery("https://app.example/cb?keep=1", map[string]string{"code": "c", "state": "", "error": "e r"})
	if got != "https://app.example/cb?code=c&error=e+r&keep=1" {
		t.Errorf("appendQuery = %q", got)
	}
}
