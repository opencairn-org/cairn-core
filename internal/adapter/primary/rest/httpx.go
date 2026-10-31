// Package rest is the JSON/HTTP primary adapter: it decodes requests, calls
// use cases and renders their results. It never touches a secondary adapter;
// handlers get use cases and port interfaces, wired in cmd/server.
package rest

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Default().Warn("writeJSON encode failed", "error", err)
	}
}

// decodeJSONBody decodes the request body into dst, capping the body size to
// guard against oversized payloads. Returns an error on malformed JSON.
func decodeJSONBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(dst)
}

// decodeOptionalJSONBody is decodeJSONBody for endpoints whose body is
// optional: an empty body leaves dst untouched, malformed JSON is still an
// error.
func decodeOptionalJSONBody(r *http.Request, dst any) error {
	err := decodeJSONBody(r, dst)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// parseIntQuery reads a non-negative integer query parameter, falling back to
// def when absent or invalid.
func parseIntQuery(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	parsed, err := strconv.Atoi(v)
	if err != nil || parsed < 0 {
		return def
	}
	return parsed
}

// requireJSON enforces a JSON Content-Type on state-changing, cookie-authed
// endpoints. This is defense-in-depth against CSRF: a cross-site HTML form can
// only send urlencoded/multipart/text bodies, never application/json, so
// rejecting non-JSON content types blocks form-based CSRF even if SameSite
// protections were ever weakened. Returns false (and writes 415) when the
// content type is not JSON; the caller should return.
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	// Trim any "; charset=..." parameter.
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if strings.TrimSpace(strings.ToLower(ct)) != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// actionErrorOf unwraps a port.ActionError when err carries one.
func actionErrorOf(err error) (*port.ActionError, bool) {
	var ae *port.ActionError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// writeActionError maps a port.ActionError to its HTTP status, a
// domain.ErrNotFound to 404; any other error is a 500 with the fallback message.
func writeActionError(w http.ResponseWriter, err error, fallback string) {
	if errors.Is(err, domain.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		http.Error(w, fallback, http.StatusInternalServerError)
		return
	}
	status := http.StatusInternalServerError
	switch ae.Kind {
	case port.ActionErrorInvalid, port.ActionErrorPrecondition:
		status = http.StatusBadRequest
	case port.ActionErrorNotFound:
		status = http.StatusNotFound
	case port.ActionErrorConflict:
		status = http.StatusConflict
	case port.ActionErrorUnavailable:
		status = http.StatusServiceUnavailable
	case port.ActionErrorUpstream:
		status = http.StatusBadGateway
	case port.ActionErrorQuota, port.ActionErrorForbidden:
		status = http.StatusForbidden
	case port.ActionErrorInternal:
	}
	http.Error(w, ae.Msg, status)
}

// clientIP is the caller's address, preferring the proxy headers.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	return r.RemoteAddr
}

// hostOnly strips the port from an address.
func hostOnly(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i > 0 && strings.Count(addr, ":") == 1 {
		addr = addr[:i]
	}
	return strings.Trim(addr, "[]")
}

// truncateUA bounds a user agent for storage.
func truncateUA(s string) string {
	const n = 200
	if len(s) <= n {
		return s
	}
	return s[:n]
}
