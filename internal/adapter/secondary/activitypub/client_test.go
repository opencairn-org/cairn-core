package activitypub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveHandle_RejectsBadHosts(t *testing.T) {
	c := NewClient()
	for _, h := range []string{
		"alice", "@alice@", "alice@",
		"alice@evil.example/path", "alice@evil.example?x=1", "alice@evil.example#f",
		"alice@user:pw@evil.example", "alice@[::1]", "alice@-bad.example", "alice@evil.example:99999x",
		"alice@" + strings.Repeat("a", 254),
	} {
		if _, err := c.ResolveHandle(context.Background(), h); err == nil {
			t.Errorf("ResolveHandle(%q) accepted", h)
		}
	}
}

func TestResolveHandle_WebFinger(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/webfinger" || r.URL.Query().Get("resource") != "acct:alice@"+r.Host {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/jrd+json")
		_, _ = w.Write([]byte(`{"links":[{"rel":"self","type":"application/activity+json","href":"https://remote.example/users/alice"}]}`))
	}))
	t.Cleanup(srv.Close)
	c := testClient(srv)

	host := strings.TrimPrefix(srv.URL, "http://")
	got, err := c.ResolveHandle(context.Background(), " @alice@"+host+" ")
	if err != nil || got != "https://remote.example/users/alice" {
		t.Fatalf("ResolveHandle = %q, %v", got, err)
	}
	if _, err := c.ResolveHandle(context.Background(), "bob@"+host); err == nil {
		t.Error("unknown account resolved")
	}
}

func TestNewClient_GuardsPrivateHosts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)
	if _, err := NewClient().FetchActor(context.Background(), srv.URL+"/users/x"); err == nil {
		t.Error("default client reached a loopback host")
	}
}
