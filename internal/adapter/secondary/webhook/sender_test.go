package webhook

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendPostsBodyAndHeaders(t *testing.T) {
	var gotBody, gotCT, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotCT, gotMethod = string(b), r.Header.Get("Content-Type"), r.Method
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	code, err := NewWithClient(srv.Client()).Send(t.Context(), srv.URL, []byte(`{"a":1}`), map[string]string{"Content-Type": "application/json"})
	if err != nil || code != http.StatusAccepted {
		t.Fatalf("send = %d %v", code, err)
	}
	if gotMethod != http.MethodPost || gotBody != `{"a":1}` || gotCT != "application/json" {
		t.Fatalf("got %s %q %q", gotMethod, gotBody, gotCT)
	}
}

func TestSendNon2xxIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if code, err := NewWithClient(srv.Client()).Send(t.Context(), srv.URL, nil, nil); err != nil || code != http.StatusInternalServerError {
		t.Fatalf("send = %d %v", code, err)
	}
}

// The production client refuses loopback targets (SSRF guard).
func TestSendBlocksLoopback(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	code, err := New().Send(t.Context(), srv.URL, nil, nil)
	if err == nil || code != 0 || hit {
		t.Fatalf("loopback must be blocked: %d %v hit=%v", code, err, hit)
	}
}
