package rest

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

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
	"github.com/opencairn-org/cairn-core/internal/usecase/oauthserver"
)

// oauthFakeDeviceStore is the OAuth repository slice the device grant touches.
type oauthFakeDeviceStore struct {
	port.OAuthServerRepo
	client domain.OAuthClient
	codes  map[string]*domain.OAuthDeviceCode // by device-code hash
	byUser map[string]string                  // user code -> hash
	tokens int
}

func newOAuthFakeDeviceStore() *oauthFakeDeviceStore {
	return &oauthFakeDeviceStore{
		client: domain.OAuthClient{
			ClientID: "cairn_dcr_tv", Name: "TV", Scopes: domain.ReadOnlyScopes,
			GrantTypes: []string{"authorization_code", "refresh_token", domain.GrantTypeDeviceCode},
		},
		codes: map[string]*domain.OAuthDeviceCode{}, byUser: map[string]string{},
	}
}

func (f *oauthFakeDeviceStore) GetClient(_ context.Context, id string) (domain.OAuthClient, error) {
	if id != f.client.ClientID {
		return domain.OAuthClient{}, domain.ErrNotFound
	}
	return f.client, nil
}
func (f *oauthFakeDeviceStore) CreateDeviceCode(_ context.Context, d domain.OAuthDeviceCode, hash []byte) error {
	if _, dup := f.byUser[d.UserCode]; dup {
		return domain.ErrUnique
	}
	d.Status = domain.DeviceCodePending
	f.codes[string(hash)] = &d
	f.byUser[d.UserCode] = string(hash)
	return nil
}
func (f *oauthFakeDeviceStore) PollDeviceCode(_ context.Context, hash []byte, now time.Time) (domain.OAuthDeviceCode, error) {
	d, ok := f.codes[string(hash)]
	if !ok || d.ConsumedAt != nil {
		return domain.OAuthDeviceCode{}, domain.ErrNotFound
	}
	out := *d
	d.LastPolledAt = &now
	return out, nil
}
func (f *oauthFakeDeviceStore) ConsumeDeviceCode(_ context.Context, hash []byte, now time.Time) (domain.OAuthDeviceCode, error) {
	d, ok := f.codes[string(hash)]
	if !ok || d.Status != domain.DeviceCodeApproved || d.ConsumedAt != nil || !now.Before(d.ExpiresAt) {
		return domain.OAuthDeviceCode{}, domain.ErrNotFound
	}
	d.ConsumedAt = &now
	return *d, nil
}
func (f *oauthFakeDeviceStore) CreateAccessToken(context.Context, domain.OAuthAccessToken, []byte) error {
	f.tokens++
	return nil
}
func (f *oauthFakeDeviceStore) CreateRefreshToken(context.Context, domain.OAuthRefreshToken, []byte) error {
	return nil
}

func (f *oauthFakeDeviceStore) decide(userCode string, approve bool, scope string) {
	d := f.codes[f.byUser[userCode]]
	uid := domain.UserID(uuid.New())
	d.UserID, d.GrantedScope = &uid, scope
	if approve {
		d.Status = domain.DeviceCodeApproved
	} else {
		d.Status = domain.DeviceCodeDenied
	}
}

func newOAuthDeviceTestHandlers(store *oauthFakeDeviceStore) *OAuthServerHandlers {
	return &OAuthServerHandlers{
		Auth: &Auth{},
		Server: &oauthserver.Server{Repo: store, Config: oauthserver.Config{
			AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour,
			DeviceCodeTTL: 10 * time.Minute, DevicePollInterval: 5 * time.Second,
		}},
		Devices: &account.DeviceAuthorizations{OAuth: store},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)), BaseURL: "https://cairn.test",
	}
}

func oauthPostForm(t *testing.T, h http.HandlerFunc, path string, form url.Values) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestOAuthDeviceAuthorizationGrant(t *testing.T) {
	store := newOAuthFakeDeviceStore()
	s := newOAuthDeviceTestHandlers(store)
	form := url.Values{"client_id": {"cairn_dcr_tv"}, "scope": {"activities:read profile:write"}}

	code, body := oauthPostForm(t, s.deviceAuthorization, "/oauth/device_authorization", form)
	if code != http.StatusOK {
		t.Fatalf("device_authorization: %d %v", code, body)
	}
	userCode, _ := body["user_code"].(string)
	deviceCode, _ := body["device_code"].(string)
	if len(userCode) != 9 || userCode[4] != '-' || !strings.HasPrefix(deviceCode, domain.OAuthDeviceCodePrefix) {
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
	if code, body := oauthPostForm(t, s.token, "/oauth/token", poll); code != http.StatusBadRequest || body["error"] != "authorization_pending" {
		t.Fatalf("pending: %d %v", code, body)
	}
	// Polling again within the interval is slowed down.
	if _, body := oauthPostForm(t, s.token, "/oauth/token", poll); body["error"] != "slow_down" {
		t.Fatalf("slow_down: %v", body)
	}
	stored.LastPolledAt = nil
	store.decide(strings.ReplaceAll(userCode, "-", ""), true, "activities:read")
	code, body = oauthPostForm(t, s.token, "/oauth/token", poll)
	if code != http.StatusOK || !strings.HasPrefix(body["access_token"].(string), domain.OAuthAccessTokenPrefix) || body["scope"] != "activities:read" || store.tokens != 1 {
		t.Fatalf("approved: %d %v", code, body)
	}
	// An approval issues tokens once.
	if _, body := oauthPostForm(t, s.token, "/oauth/token", poll); body["error"] != "invalid_grant" {
		t.Fatalf("replay: %v", body)
	}
}

func TestOAuthDeviceAuthorizationRefusals(t *testing.T) {
	store := newOAuthFakeDeviceStore()
	s := newOAuthDeviceTestHandlers(store)
	if code, body := oauthPostForm(t, s.deviceAuthorization, "/oauth/device_authorization", url.Values{"client_id": {"nope"}}); code != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("unknown client: %d %v", code, body)
	}
	store.client.GrantTypes = []string{"authorization_code"}
	if _, body := oauthPostForm(t, s.deviceAuthorization, "/oauth/device_authorization", url.Values{"client_id": {"cairn_dcr_tv"}}); body["error"] != "unauthorized_client" {
		t.Fatalf("grant not allowed: %v", body)
	}
	store.client.GrantTypes = []string{domain.GrantTypeDeviceCode}
	_, body := oauthPostForm(t, s.deviceAuthorization, "/oauth/device_authorization", url.Values{"client_id": {"cairn_dcr_tv"}})
	deviceCode := body["device_code"].(string)
	userCode := strings.ReplaceAll(body["user_code"].(string), "-", "")
	poll := url.Values{"grant_type": {domain.GrantTypeDeviceCode}, "client_id": {"cairn_dcr_tv"}, "device_code": {deviceCode}}

	store.decide(userCode, false, "")
	if _, body := oauthPostForm(t, s.token, "/oauth/token", poll); body["error"] != "access_denied" {
		t.Fatalf("denied: %v", body)
	}
	store.codes[store.byUser[userCode]].LastPolledAt = nil
	store.codes[store.byUser[userCode]].ExpiresAt = time.Now().Add(-time.Second)
	if _, body := oauthPostForm(t, s.token, "/oauth/token", poll); body["error"] != "expired_token" {
		t.Fatalf("expired: %v", body)
	}
	if _, body := oauthPostForm(t, s.token, "/oauth/token", url.Values{"grant_type": {domain.GrantTypeDeviceCode}, "client_id": {"cairn_dcr_tv"}, "device_code": {"cairn_dc_unknown"}}); body["error"] != "invalid_grant" {
		t.Fatalf("unknown code: %v", body)
	}
}

func TestOAuthDeviceVerificationRefusesOAuthTokens(t *testing.T) {
	s := newOAuthDeviceTestHandlers(newOAuthFakeDeviceStore())
	req := httptest.NewRequest(http.MethodGet, "/oauth/device/info?user_code=1234-5678", nil)
	req.Header.Set("Authorization", "Bearer cairn_at_x")
	rec := httptest.NewRecorder()
	s.deviceInfo(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("oauth token on the browser endpoint: %d", rec.Code)
	}
}

func TestOAuthDeviceEndpointsRateLimited(t *testing.T) {
	s := newOAuthDeviceTestHandlers(newOAuthFakeDeviceStore())
	s.DeviceLimit = func(string) bool { return false }
	mux := http.NewServeMux()
	s.Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/oauth/device_authorization", nil))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "10" {
		t.Fatalf("limited: %d", rec.Code)
	}
}
