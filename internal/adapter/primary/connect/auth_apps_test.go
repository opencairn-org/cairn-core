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

type fakeOAuthGrants struct {
	port.OAuthGrantStore
	grants []domain.OAuthGrant
}

func (f *fakeOAuthGrants) ListGrantsForUser(context.Context, domain.UserID, time.Time) ([]domain.OAuthGrant, error) {
	return f.grants, nil
}
func (f *fakeOAuthGrants) RevokeClientForUser(_ context.Context, _ domain.UserID, clientID string) (int, error) {
	if clientID == "app-1" {
		return 3, nil
	}
	return 0, nil
}

func TestAuthorizedAppsRPC(t *testing.T) {
	s := &AuthServer{Apps: &account.AuthorizedApps{OAuth: &fakeOAuthGrants{grants: []domain.OAuthGrant{{ClientID: "app-1", ClientName: "Phone", Scopes: []string{"activities:read"}, ActiveTokens: 3}}}}}
	ctx := context.WithValue(t.Context(), ctxKeyUserID, domain.UserID(uuid.New()))
	if _, err := (&AuthServer{}).ListAuthorizedApps(ctx, &v1.ListAuthorizedAppsRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("unwired: %v", err)
	}
	list, err := s.ListAuthorizedApps(ctx, &v1.ListAuthorizedAppsRequest{})
	if err != nil || len(list.GetApps()) != 1 || list.GetApps()[0].GetClientName() != "Phone" || list.GetApps()[0].GetActiveTokens() != 3 {
		t.Fatalf("list %v %v", list, err)
	}
	res, err := s.RevokeAuthorizedApp(ctx, &v1.RevokeAuthorizedAppRequest{ClientId: "app-1"})
	if err != nil || res.GetTokensRevoked() != 3 {
		t.Fatalf("revoke %v %v", res, err)
	}
	if _, err := s.RevokeAuthorizedApp(ctx, &v1.RevokeAuthorizedAppRequest{ClientId: "other"}); connectrpc.CodeOf(err) != connectrpc.CodeNotFound {
		t.Fatalf("unknown: %v", err)
	}
}
