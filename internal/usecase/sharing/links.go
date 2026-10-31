// Package sharing manages activity share links: unguessable tokens that open
// one activity to the "link" audience.
package sharing

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ErrUnavailable: share links are not wired on this instance.
var ErrUnavailable = port.NewActionError(port.ActionErrorUnavailable, "share links are not available")

// Links is the owner's share-link management plus token checks for readers.
type Links struct {
	Repo       port.ShareLinkRepo
	Activities port.ActivityRepo
}

// owned loads the caller's live activity; anything else is not found.
func (l *Links) owned(ctx context.Context, owner domain.UserID, id domain.ActivityID) error {
	a, err := l.Activities.GetActivity(ctx, id)
	if err != nil || a.UserID != owner || a.IsDeleted() {
		return domain.ErrNotFound
	}
	return nil
}

// Create mints a link for the owner's activity and returns its token.
func (l *Links) Create(ctx context.Context, owner domain.UserID, id domain.ActivityID) (string, error) {
	if l == nil || l.Repo == nil {
		return "", ErrUnavailable
	}
	if err := l.owned(ctx, owner, id); err != nil {
		return "", err
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if err := l.Repo.Create(ctx, domain.ShareLink{Token: token, ActivityID: id, CreatedBy: owner}); err != nil {
		return "", fmt.Errorf("create share link: %w", err)
	}
	return token, nil
}

// List returns the links of the owner's activity.
func (l *Links) List(ctx context.Context, owner domain.UserID, id domain.ActivityID) ([]domain.ShareLink, error) {
	if l == nil || l.Repo == nil {
		return nil, ErrUnavailable
	}
	if err := l.owned(ctx, owner, id); err != nil {
		return nil, err
	}
	return l.Repo.ListForActivity(ctx, id)
}

// Revoke disables the owner's link.
func (l *Links) Revoke(ctx context.Context, owner domain.UserID, token string) error {
	if l == nil || l.Repo == nil {
		return ErrUnavailable
	}
	return l.Repo.Revoke(ctx, token, owner)
}

// Grants reports whether token is an active link to activity id.
func (l *Links) Grants(ctx context.Context, token string, id domain.ActivityID) bool {
	if l == nil || l.Repo == nil || token == "" {
		return false
	}
	link, err := l.Repo.GetActive(ctx, token)
	return err == nil && link.ActivityID == id
}

// newToken returns a 24-byte URL-safe random token (~32 chars).
func newToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("share token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
