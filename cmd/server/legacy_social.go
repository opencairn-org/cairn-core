package main

import (
	"context"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/social"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// Leftovers of the social group still used by handlers in this package.

// optionalSessionUser is the session user; ok=false means anonymous.
func optionalSessionUser(r *http.Request, app *App) (domain.UserID, bool) {
	return resolveSessionUser(r, app)
}

// activityQuotaStatus is view.Quota's usage; a counting failure reads as no
// usage so a transient DB hiccup never wrongly blocks an upload.
func activityQuotaStatus(ctx context.Context, app *App, userID domain.UserID) view.QuotaStatus {
	st, _ := app.QuotaViews.Status(ctx, userID)
	return st
}

// truncateUTF8 caps s at maxBytes without splitting a rune.
func truncateUTF8(s string, maxBytes int) string { return social.TruncateUTF8(s, maxBytes) }

// resolveModeratorUser (admin_api.go) lost its last caller with the moderation move; drop both together.
var _ = resolveModeratorUser
