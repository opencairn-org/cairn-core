package connect

import (
	"context"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
)

type fakeDeviceCodes struct {
	port.OAuthClientStore
	port.OAuthDeviceCodeStore
	code    domain.OAuthDeviceCode
	granted string
}

func (f *fakeDeviceCodes) GetClient(context.Context, string) (domain.OAuthClient, error) {
	return domain.OAuthClient{ClientID: "tv", Name: "Living room TV", Scopes: domain.SupportedScopes}, nil
}
func (f *fakeDeviceCodes) FindDeviceCodeByUserCode(_ context.Context, code string, _ time.Time) (domain.OAuthDeviceCode, error) {
	if code != f.code.UserCode {
		return domain.OAuthDeviceCode{}, domain.ErrNotFound
	}
	return f.code, nil
}
func (f *fakeDeviceCodes) DecideDeviceCode(_ context.Context, _ string, _ domain.UserID, approve bool, granted string, _ time.Time) error {
	f.granted = granted
	if approve {
		f.code.Status = domain.DeviceCodeApproved
	} else {
		f.code.Status = domain.DeviceCodeDenied
	}
	return nil
}

func TestDeviceAuthorizationRPC(t *testing.T) {
	repo := &fakeDeviceCodes{code: domain.OAuthDeviceCode{
		ClientID: "tv", UserCode: "12345678", Scope: "activities:read activities:write",
		Status: domain.DeviceCodePending, ExpiresAt: time.Now().Add(time.Minute),
	}}
	s := &AuthServer{Devices: &account.DeviceAuthorizations{OAuth: repo}}
	uid := domain.UserID(uuid.New())
	if _, err := s.LookupDeviceAuthorization(t.Context(), &v1.LookupDeviceAuthorizationRequest{UserCode: "1234-5678"}); connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
	if _, err := (&AuthServer{}).LookupDeviceAuthorization(context.WithValue(t.Context(), ctxKeyUserID, uid), &v1.LookupDeviceAuthorizationRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("unwired: %v", err)
	}
	// Cookie session: unrestricted, sees both scopes.
	session := context.WithValue(t.Context(), ctxKeyUserID, uid)
	look, err := s.LookupDeviceAuthorization(session, &v1.LookupDeviceAuthorizationRequest{UserCode: "1234-5678"})
	if err != nil || look.GetClientName() != "Living room TV" || len(look.GetScopes()) != 2 || look.GetScopes()[1].GetWrite() != true {
		t.Fatalf("session lookup %v %v", look, err)
	}
	if _, err := s.LookupDeviceAuthorization(session, &v1.LookupDeviceAuthorizationRequest{UserCode: "0000-0000"}); connectrpc.CodeOf(err) != connectrpc.CodeNotFound {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := s.LookupDeviceAuthorization(session, &v1.LookupDeviceAuthorizationRequest{UserCode: "12"}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("format: %v", err)
	}
	// OAuth token with read scopes: the grant is capped to them.
	oauth := context.WithValue(session, ctxKeyScopes, []string{"activities:read", "devices:write"})
	look, err = s.LookupDeviceAuthorization(oauth, &v1.LookupDeviceAuthorizationRequest{UserCode: "12345678"})
	if err != nil || len(look.GetScopes()) != 1 || look.GetScopes()[0].GetScope() != "activities:read" {
		t.Fatalf("capped lookup %v %v", look, err)
	}
	dec, err := s.DecideDeviceAuthorization(oauth, &v1.DecideDeviceAuthorizationRequest{UserCode: "1234 5678", Approve: true})
	if err != nil || len(dec.GetGrantedScopes()) != 1 || repo.granted != "activities:read" {
		t.Fatalf("decide %v %v %q", dec, err, repo.granted)
	}
	if _, err := s.DecideDeviceAuthorization(session, &v1.DecideDeviceAuthorizationRequest{UserCode: "1234-5678", Approve: false}); connectrpc.CodeOf(err) != connectrpc.CodeNotFound {
		t.Fatalf("already decided: %v", err)
	}
}

func TestScopesFromHandlerCtx(t *testing.T) {
	if scopesFromHandlerCtx(t.Context()) != nil {
		t.Fatal("session must be unrestricted (nil)")
	}
	empty := scopesFromHandlerCtx(context.WithValue(t.Context(), ctxKeyScopes, []string(nil)))
	if empty == nil || len(empty) != 0 {
		t.Fatalf("scopeless token must not be unrestricted: %v", empty)
	}
}

func TestScopeCheckDeviceFamily(t *testing.T) {
	if err := scopeCheck("/cairn.v1.AuthService/LookupDeviceAuthorization", []string{"profile:read"}); connectrpc.CodeOf(err) != connectrpc.CodePermissionDenied {
		t.Fatalf("profile scope must not look devices up: %v", err)
	}
	if err := scopeCheck("/cairn.v1.AuthService/LookupDeviceAuthorization", []string{"devices:read"}); err != nil {
		t.Fatalf("devices:read: %v", err)
	}
	if err := scopeCheck("/cairn.v1.AuthService/DecideDeviceAuthorization", []string{"devices:read"}); connectrpc.CodeOf(err) != connectrpc.CodePermissionDenied {
		t.Fatalf("read must not decide: %v", err)
	}
	if err := scopeCheck("/cairn.v1.AuthService/DecideDeviceAuthorization", []string{"devices:write"}); err != nil {
		t.Fatalf("devices:write: %v", err)
	}
}
