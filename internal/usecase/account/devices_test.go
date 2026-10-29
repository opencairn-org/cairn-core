package account

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeDeviceRepo struct {
	port.OAuthServerRepo
	codes    map[string]domain.OAuthDeviceCode
	client   domain.OAuthClient
	decided  []string
	approved bool
	granted  string
}

func (f *fakeDeviceRepo) GetClient(context.Context, string) (domain.OAuthClient, error) {
	return f.client, nil
}
func (f *fakeDeviceRepo) FindDeviceCodeByUserCode(_ context.Context, code string, now time.Time) (domain.OAuthDeviceCode, error) {
	d, ok := f.codes[code]
	if !ok || !d.IsLiveAt(now) {
		return domain.OAuthDeviceCode{}, domain.ErrNotFound
	}
	return d, nil
}
func (f *fakeDeviceRepo) DecideDeviceCode(_ context.Context, code string, _ domain.UserID, approve bool, granted string, _ time.Time) error {
	d, ok := f.codes[code]
	if !ok || d.Status != domain.DeviceCodePending {
		return domain.ErrNotFound
	}
	f.decided = append(f.decided, code)
	f.approved, f.granted = approve, granted
	if approve {
		d.Status = domain.DeviceCodeApproved
	} else {
		d.Status = domain.DeviceCodeDenied
	}
	f.codes[code] = d
	return nil
}

func TestDeviceAuthorizations(t *testing.T) {
	now := time.Now()
	repo := &fakeDeviceRepo{
		client: domain.OAuthClient{ClientID: "c1", Name: "TV", Scopes: []string{domain.ScopeActivitiesRead, domain.ScopeActivitiesWrite}},
		codes: map[string]domain.OAuthDeviceCode{
			"12345678": {ClientID: "c1", UserCode: "12345678", Scope: "activities:read activities:write", Status: domain.DeviceCodePending, ExpiresAt: now.Add(time.Minute)},
			"00000000": {ClientID: "c1", UserCode: "00000000", Status: domain.DeviceCodePending, ExpiresAt: now.Add(-time.Minute)},
		},
	}
	d := &DeviceAuthorizations{OAuth: repo, Now: func() time.Time { return now }}
	uid := domain.UserID(uuid.New())

	if _, err := d.Lookup(t.Context(), "12-34", nil); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("format: %v", err)
	}
	if _, err := d.Lookup(t.Context(), "0000-0000", nil); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("expired: %v", err)
	}
	// An unrestricted approver sees the full request; an OAuth approver only its own scopes.
	full, err := d.Lookup(t.Context(), "1234-5678", nil)
	if err != nil || full.Client.Name != "TV" || len(full.Scopes) != 2 {
		t.Fatalf("lookup %+v %v", full, err)
	}
	capped, err := d.Lookup(t.Context(), "1234 5678", []string{domain.ScopeActivitiesRead, domain.ScopeDevicesWrite})
	if err != nil || len(capped.Scopes) != 1 || capped.Scopes[0] != domain.ScopeActivitiesRead {
		t.Fatalf("capped %+v %v", capped.Scopes, err)
	}
	granted, err := d.Decide(t.Context(), uid, "1234-5678", true, []string{domain.ScopeActivitiesRead})
	if err != nil || len(granted) != 1 || !repo.approved || repo.granted != "activities:read" {
		t.Fatalf("approve %v %v %+v", granted, err, repo)
	}
	// Decided once: a second decision is not found.
	if _, err := d.Decide(t.Context(), uid, "1234-5678", false, nil); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("twice: %v", err)
	}
}

func TestDeviceAuthorizationsDeny(t *testing.T) {
	now := time.Now()
	repo := &fakeDeviceRepo{
		client: domain.OAuthClient{ClientID: "c1", Scopes: domain.ReadOnlyScopes},
		codes:  map[string]domain.OAuthDeviceCode{"11112222": {ClientID: "c1", UserCode: "11112222", Status: domain.DeviceCodePending, ExpiresAt: now.Add(time.Minute)}},
	}
	d := &DeviceAuthorizations{OAuth: repo, Now: func() time.Time { return now }}
	granted, err := d.Decide(t.Context(), domain.UserID(uuid.New()), "1111-2222", false, nil)
	if err != nil || len(granted) != 0 || repo.approved || repo.granted != "" {
		t.Fatalf("deny %v %v %+v", granted, err, repo)
	}
}
