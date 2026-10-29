package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
)

// fakeDeviceStore is the OAuth repository slice the device grant touches.
type fakeDeviceStore struct {
	port.OAuthServerRepo
	client domain.OAuthClient
	codes  map[string]*domain.OAuthDeviceCode // by device-code hash
	byUser map[string]string                  // user code -> hash
	tokens int
}

func newFakeDeviceStore() *fakeDeviceStore {
	return &fakeDeviceStore{
		client: domain.OAuthClient{
			ClientID: "cairn_dcr_tv", Name: "TV", Scopes: domain.ReadOnlyScopes,
			GrantTypes: []string{"authorization_code", "refresh_token", domain.GrantTypeDeviceCode},
		},
		codes: map[string]*domain.OAuthDeviceCode{}, byUser: map[string]string{},
	}
}

func (f *fakeDeviceStore) GetClient(_ context.Context, id string) (domain.OAuthClient, error) {
	if id != f.client.ClientID {
		return domain.OAuthClient{}, domain.ErrNotFound
	}
	return f.client, nil
}
func (f *fakeDeviceStore) CreateDeviceCode(_ context.Context, d domain.OAuthDeviceCode, hash []byte) error {
	if _, dup := f.byUser[d.UserCode]; dup {
		return domain.ErrUnique
	}
	d.Status = domain.DeviceCodePending
	f.codes[string(hash)] = &d
	f.byUser[d.UserCode] = string(hash)
	return nil
}
func (f *fakeDeviceStore) PollDeviceCode(_ context.Context, hash []byte, now time.Time) (domain.OAuthDeviceCode, error) {
	d, ok := f.codes[string(hash)]
	if !ok || d.ConsumedAt != nil {
		return domain.OAuthDeviceCode{}, domain.ErrNotFound
	}
	out := *d
	d.LastPolledAt = &now
	return out, nil
}
func (f *fakeDeviceStore) ConsumeDeviceCode(_ context.Context, hash []byte, now time.Time) (domain.OAuthDeviceCode, error) {
	d, ok := f.codes[string(hash)]
	if !ok || d.Status != domain.DeviceCodeApproved || d.ConsumedAt != nil || !now.Before(d.ExpiresAt) {
		return domain.OAuthDeviceCode{}, domain.ErrNotFound
	}
	d.ConsumedAt = &now
	return *d, nil
}
func (f *fakeDeviceStore) CreateAccessToken(context.Context, domain.OAuthAccessToken, []byte) error {
	f.tokens++
	return nil
}
func (f *fakeDeviceStore) CreateRefreshToken(context.Context, domain.OAuthRefreshToken, []byte) error {
	return nil
}

func (f *fakeDeviceStore) decide(userCode string, approve bool, scope string) {
	d := f.codes[f.byUser[userCode]]
	uid := domain.UserID(uuid.New())
	d.UserID, d.GrantedScope = &uid, scope
	if approve {
		d.Status = domain.DeviceCodeApproved
	} else {
		d.Status = domain.DeviceCodeDenied
	}
}

func newDeviceTestServer(store *fakeDeviceStore) *oauthServer {
	return &oauthServer{
		oauth: store, devices: &account.DeviceAuthorizations{OAuth: store},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), baseURL: "https://cairn.test",
		auth: config.AuthConfig{
			OAuthAccessTokenTTL: time.Hour, OAuthRefreshTokenTTL: time.Hour,
			OAuthDeviceCodeTTL: 10 * time.Minute, OAuthDevicePollInterval: 5 * time.Second,
		},
	}
}

func postForm(t *testing.T, h http.HandlerFunc, path string, form url.Values) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestDeviceAuthorizationGrant(t *testing.T) {
	store := newFakeDeviceStore()
	s := newDeviceTestServer(store)
	form := url.Values{"client_id": {"cairn_dcr_tv"}, "scope": {"activities:read profile:write"}}

	code, body := postForm(t, s.deviceAuthorization, "/oauth/device_authorization", form)
	if code != http.StatusOK {
		t.Fatalf("device_authorization: %d %v", code, body)
	}
	userCode, _ := body["user_code"].(string)
	deviceCode, _ := body["device_code"].(string)
	if len(userCode) != 9 || userCode[4] != '-' || !strings.HasPrefix(deviceCode, oauthDeviceCodePrefix) {
		t.Fatalf("codes: %q %q", userCode, deviceCode)
	}
	if body["verification_uri"] != "https://cairn.test/oauth/device" || body["interval"].(float64) != 5 || body["expires_in"].(float64) != 600 {
		t.Fatalf("metadata: %v", body)
	}
	if body["verification_uri_complete"] != "https://cairn.test/oauth/device?user_code="+userCode {
		t.Fatalf("complete uri: %v", body["verification_uri_complete"])
	}
	// The stored scope is the request filtered to the client's (read-only) scopes.
	stored := store.codes[store.byUser[strings.ReplaceAll(userCode, "-", "")]]
	if stored.Scope != "activities:read" {
		t.Fatalf("stored scope %q", stored.Scope)
	}

	poll := url.Values{"grant_type": {domain.GrantTypeDeviceCode}, "client_id": {"cairn_dcr_tv"}, "device_code": {deviceCode}}
	if code, body := postForm(t, s.token, "/oauth/token", poll); code != http.StatusBadRequest || body["error"] != "authorization_pending" {
		t.Fatalf("pending: %d %v", code, body)
	}
	// Polling again within the interval is slowed down.
	if _, body := postForm(t, s.token, "/oauth/token", poll); body["error"] != "slow_down" {
		t.Fatalf("slow_down: %v", body)
	}
	stored.LastPolledAt = nil
	store.decide(strings.ReplaceAll(userCode, "-", ""), true, "activities:read")
	code, body = postForm(t, s.token, "/oauth/token", poll)
	if code != http.StatusOK || !strings.HasPrefix(body["access_token"].(string), oauthAccessTokenPrefix) || body["scope"] != "activities:read" || store.tokens != 1 {
		t.Fatalf("approved: %d %v", code, body)
	}
	// An approval issues tokens once.
	if _, body := postForm(t, s.token, "/oauth/token", poll); body["error"] != "invalid_grant" {
		t.Fatalf("replay: %v", body)
	}
}

func TestDeviceAuthorizationRefusals(t *testing.T) {
	store := newFakeDeviceStore()
	s := newDeviceTestServer(store)
	if _, body := postForm(t, s.deviceAuthorization, "/oauth/device_authorization", url.Values{"client_id": {"nope"}}); body["error"] != "invalid_client" {
		t.Fatalf("unknown client: %v", body)
	}
	store.client.GrantTypes = []string{"authorization_code"}
	if _, body := postForm(t, s.deviceAuthorization, "/oauth/device_authorization", url.Values{"client_id": {"cairn_dcr_tv"}}); body["error"] != "unauthorized_client" {
		t.Fatalf("grant not allowed: %v", body)
	}
	store.client.GrantTypes = []string{domain.GrantTypeDeviceCode}
	_, body := postForm(t, s.deviceAuthorization, "/oauth/device_authorization", url.Values{"client_id": {"cairn_dcr_tv"}})
	deviceCode := body["device_code"].(string)
	userCode := strings.ReplaceAll(body["user_code"].(string), "-", "")
	poll := url.Values{"grant_type": {domain.GrantTypeDeviceCode}, "client_id": {"cairn_dcr_tv"}, "device_code": {deviceCode}}

	store.decide(userCode, false, "")
	if _, body := postForm(t, s.token, "/oauth/token", poll); body["error"] != "access_denied" {
		t.Fatalf("denied: %v", body)
	}
	store.codes[store.byUser[userCode]].LastPolledAt = nil
	store.codes[store.byUser[userCode]].ExpiresAt = time.Now().Add(-time.Second)
	if _, body := postForm(t, s.token, "/oauth/token", poll); body["error"] != "expired_token" {
		t.Fatalf("expired: %v", body)
	}
	if _, body := postForm(t, s.token, "/oauth/token", url.Values{"grant_type": {domain.GrantTypeDeviceCode}, "client_id": {"cairn_dcr_tv"}, "device_code": {"cairn_dc_unknown"}}); body["error"] != "invalid_grant" {
		t.Fatalf("unknown code: %v", body)
	}
}

func TestDeviceVerificationRefusesOAuthTokens(t *testing.T) {
	s := newDeviceTestServer(newFakeDeviceStore())
	req := httptest.NewRequest(http.MethodGet, "/oauth/device/info?user_code=1234-5678", nil)
	req.Header.Set("Authorization", "Bearer cairn_at_x")
	rec := httptest.NewRecorder()
	s.deviceInfo(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("oauth token on the browser endpoint: %d", rec.Code)
	}
}
