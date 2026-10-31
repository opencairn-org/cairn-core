package account

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeOAuth struct {
	port.OAuthGrantStore
	grants  []domain.OAuthGrant
	revoked map[string]int
}

func (f *fakeOAuth) ListGrantsForUser(context.Context, domain.UserID, time.Time) ([]domain.OAuthGrant, error) {
	return f.grants, nil
}
func (f *fakeOAuth) RevokeClientForUser(_ context.Context, _ domain.UserID, clientID string) (int, error) {
	return f.revoked[clientID], nil
}

func TestAuthorizedApps(t *testing.T) {
	repo := &fakeOAuth{grants: []domain.OAuthGrant{{ClientID: "app-1", ClientName: "Phone", Scopes: []string{"activities:read"}}}, revoked: map[string]int{"app-1": 2}}
	a := &AuthorizedApps{OAuth: repo}
	uid := domain.UserID(uuid.New())
	list, err := a.List(t.Context(), uid)
	if err != nil || len(list) != 1 || list[0].ClientName != "Phone" {
		t.Fatalf("list %v %v", list, err)
	}
	if _, err := a.Revoke(t.Context(), uid, " "); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("blank: %v", err)
	}
	if _, err := a.Revoke(t.Context(), uid, "unknown"); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("unknown: %v", err)
	}
	if n, err := a.Revoke(t.Context(), uid, "app-1"); err != nil || n != 2 {
		t.Fatalf("revoke: %d %v", n, err)
	}
}
