package rest

import (
	"errors"
	"io"
	"net/http"

	syncuc "github.com/opencairn-org/cairn-core/internal/usecase/sync"
)

// WebhookHandlers are the provider-agnostic webhook endpoints. The path
// token is the pool's worker key (<name>-<provider>), the token of its NATS
// webhook subjects; which pools have one is up to the connected workers
// (they advertise it in presence). Core only relays.
//
//	POST /webhooks/{worker_key}   raw forward to cairn.webhooks.<worker_key>.event
//	GET  /webhooks/{worker_key}   request/reply to cairn.webhooks.<worker_key>.verify
type WebhookHandlers struct {
	// Forwarder nil (NATS not wired) answers 503.
	Forwarder *syncuc.WebhookForwarder
	// Allow is the per-IP rate limit; these endpoints are unauthenticated.
	Allow func(ip string) bool
}

// syncWebhookMaxBody caps a POSTed webhook; providers send <10 KB.
const syncWebhookMaxBody = 1 << 20

// Mount registers the routes.
func (h *WebhookHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/{worker_key}", h.limited(h.post))
	mux.HandleFunc("GET /webhooks/{worker_key}", h.limited(h.get))
}

// limited applies the rate limit, the NATS gate and the worker-key check.
func (h *WebhookHandlers) limited(next func(w http.ResponseWriter, r *http.Request, workerKey string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.Allow != nil && !h.Allow(clientIP(r)) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		if h.Forwarder == nil {
			http.Error(w, "webhook forwarder not wired: NATS adapter not running", http.StatusServiceUnavailable)
			return
		}
		workerKey := r.PathValue("worker_key")
		if !syncuc.ValidWebhookTarget(workerKey) {
			http.Error(w, "invalid worker key", http.StatusBadRequest)
			return
		}
		next(w, r, workerKey)
	}
}

func (h *WebhookHandlers) post(w http.ResponseWriter, r *http.Request, workerKey string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, syncWebhookMaxBody))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	err = h.Forwarder.Forward(r.Context(), workerKey, syncFlattenHeaders(r.Header), r.RemoteAddr, body)
	switch {
	case errors.Is(err, syncuc.ErrWebhookForward):
		http.Error(w, "forward failed", http.StatusBadGateway)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (h *WebhookHandlers) get(w http.ResponseWriter, r *http.Request, workerKey string) {
	resp, err := h.Forwarder.Verify(r.Context(), workerKey, r.Method, syncFlattenQuery(r.URL.Query()))
	switch {
	case errors.Is(err, syncuc.ErrWebhookNoWorker):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	case errors.Is(err, syncuc.ErrWebhookBadReply):
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", resp.ContentType)
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// syncFlattenHeaders keeps the first value of each header under a lowercased
// key; providers don't repeat header names.
func syncFlattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if len(vals) > 0 {
			out[syncLowerASCII(k)] = vals[0]
		}
	}
	return out
}

// syncFlattenQuery keeps the first value of each query parameter.
func syncFlattenQuery(q map[string][]string) map[string]string {
	out := make(map[string]string, len(q))
	for k, vals := range q {
		if len(vals) > 0 {
			out[k] = vals[0]
		}
	}
	return out
}

func syncLowerASCII(s string) string {
	b := make([]byte, len(s))
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
