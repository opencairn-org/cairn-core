package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// notifTypeLabel is the UI copy for the user-facing notification types
// (domain.UserFacingNotificationTypes carries the order).
var notifTypeLabel = map[domain.NotificationType]string{
	domain.NotificationTypeSegmentPersonalRecord:        "Segment personal records",
	domain.NotificationTypeSegmentInstanceRecord:        "Segment course records",
	domain.NotificationTypeActivityImported:             "Activity imported",
	domain.NotificationTypeActivityReimported:           "Activity updated from source",
	domain.NotificationTypeWorkerOffline:                "Connector went offline",
	domain.NotificationTypeExternalAccountRefreshFailed: "Connection needs re-auth",
	domain.NotificationTypeGearDecisionNeeded:           "Gear needs a decision",
}

// mountNotificationPrefs exposes the per-user email notification preference
// matrix. v1 is the email channel (in-app is always on; webhook/push later).
//
//	GET /api/notifications/preferences        → [{event_type,key,label,email_enabled}]
//	PUT /api/notifications/preferences        { event_type, channel, enabled }
func mountNotificationPrefs(mux *http.ServeMux, app *App, logger *slog.Logger) {
	mux.HandleFunc("GET /api/notifications/preferences", notifPrefsHandleGet(app))

	mux.HandleFunc("PUT /api/notifications/preferences", notifPrefsHandlePut(app, logger))

	logger.Info("notification preferences endpoints mounted")
}

// notifPrefsHandleGet serves GET /api/notifications/preferences.
func notifPrefsHandleGet(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		cells, err := app.NotificationPreferences.Matrix(r.Context(), userID, domain.NotificationChannelEmail)
		if err != nil {
			writeActionError(w, err, "load failed")
			return
		}
		out := make([]map[string]any, 0, len(cells))
		for _, c := range cells {
			out = append(out, map[string]any{
				"event_type":    int(c.Type),
				"key":           c.Type.String(),
				"label":         notifTypeLabel[c.Type],
				"email_enabled": c.Enabled,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"preferences":   out,
			"email_enabled": app.Email != nil, // instance has an email channel at all
		})
	}
}

// notifPrefsHandlePut serves PUT /api/notifications/preferences.
func notifPrefsHandlePut(app *App, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		if !requireJSON(w, r) {
			return
		}
		var body struct {
			EventType int    `json:"event_type"`
			Channel   string `json:"channel"`
			Enabled   bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if err := app.NotificationPreferences.Set(r.Context(), userID, domain.NotificationType(body.EventType), domain.NotificationChannel(body.Channel), body.Enabled); err != nil {
			logger.Warn("notification preference not saved", "user", userID, "error", err)
			writeActionError(w, err, "save failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
