package rest

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/safehttp"
	"github.com/opencairn-org/cairn-core/internal/usecase/notification"
)

// NotificationSettingsHandlers serve the user's side-channel notification
// settings. Routes whose port is absent from Settings (or a nil Webhooks)
// stay unmounted.
//
//	GET  /api/notifications/preferences   → [{event_type,key,label,email_enabled}]
//	PUT  /api/notifications/preferences   { event_type, channel, enabled }
//	GET  /api/notifications/quiet-hours
//	PUT  /api/notifications/quiet-hours   { enabled, start_minute, end_minute, days_of_week, tz }
//	GET  /api/notifications/deliveries?limit=
//	POST /api/notifications/test-email
//	GET    /api/notifications/webhooks
//	POST   /api/notifications/webhooks                    → {…, secret} (once)
//	PUT    /api/notifications/webhooks/{id}
//	POST   /api/notifications/webhooks/{id}/rotate-secret → {secret} (once)
//	POST   /api/notifications/webhooks/{id}/test
//	DELETE /api/notifications/webhooks/{id}
type NotificationSettingsHandlers struct {
	Auth        *Auth
	Preferences *notification.Preferences
	Settings    *notification.Settings
	Webhooks    *notification.Webhooks
	Log         *slog.Logger
}

// Mount registers the routes.
func (h *NotificationSettingsHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/notifications/test-email", h.testEmail)
	mux.HandleFunc("GET /api/notifications/preferences", h.getPreferences)
	mux.HandleFunc("PUT /api/notifications/preferences", h.putPreference)
	if h.Webhooks != nil {
		mux.HandleFunc("GET /api/notifications/webhooks", h.listWebhooks)
		mux.HandleFunc("POST /api/notifications/webhooks", h.createWebhook)
		mux.HandleFunc("PUT /api/notifications/webhooks/{id}", h.updateWebhook)
		mux.HandleFunc("POST /api/notifications/webhooks/{id}/rotate-secret", h.rotateWebhookSecret)
		mux.HandleFunc("POST /api/notifications/webhooks/{id}/test", h.testWebhook)
		mux.HandleFunc("DELETE /api/notifications/webhooks/{id}", h.deleteWebhook)
	}
	if h.Settings.QuietHours != nil {
		mux.HandleFunc("GET /api/notifications/quiet-hours", h.getQuietHours)
		mux.HandleFunc("PUT /api/notifications/quiet-hours", h.putQuietHours)
	}
	if h.Settings.Deliveries != nil {
		mux.HandleFunc("GET /api/notifications/deliveries", h.deliveries)
	}
}

func (h *NotificationSettingsHandlers) testEmail(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	to, err := h.Settings.SendTestEmail(r.Context(), userID)
	if err != nil {
		if ae, ok := actionErrorOf(err); ok && ae.Kind == port.ActionErrorUpstream {
			h.Log.Warn("test email send failed", "user", userID, "error", err)
		}
		writeActionError(w, err, "send failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "to": to})
}

// acctNotifTypeLabel is the UI copy for the user-facing notification types
// (domain.UserFacingNotificationTypes carries the order).
var acctNotifTypeLabel = map[domain.NotificationType]string{
	domain.NotificationTypeSegmentPersonalRecord:        "Segment personal records",
	domain.NotificationTypeSegmentInstanceRecord:        "Segment course records",
	domain.NotificationTypeActivityImported:             "Activity imported",
	domain.NotificationTypeActivityReimported:           "Activity updated from source",
	domain.NotificationTypeWorkerOffline:                "Connector went offline",
	domain.NotificationTypeExternalAccountRefreshFailed: "Connection needs re-auth",
	domain.NotificationTypeGearDecisionNeeded:           "Gear needs a decision",
}

func (h *NotificationSettingsHandlers) getPreferences(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	cells, err := h.Preferences.Matrix(r.Context(), userID, domain.NotificationChannelEmail)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(cells))
	for _, c := range cells {
		out = append(out, map[string]any{
			"event_type":    int(c.Type),
			"key":           c.Type.String(),
			"label":         acctNotifTypeLabel[c.Type],
			"email_enabled": c.Enabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"preferences":   out,
		"email_enabled": h.Settings.Mail != nil, // instance has an email channel at all
	})
}

func (h *NotificationSettingsHandlers) putPreference(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
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
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.Preferences.Set(r.Context(), userID, domain.NotificationType(body.EventType), domain.NotificationChannel(body.Channel), body.Enabled); err != nil {
		h.Log.Warn("notification preference not saved", "user", userID, "error", err)
		writeActionError(w, err, "save failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *NotificationSettingsHandlers) getQuietHours(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	q, err := h.Settings.QuietHoursOf(r.Context(), userID)
	if err != nil {
		h.Log.Error("get quiet hours failed", "user", userID, "error", err)
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	days := q.DaysOfWeek
	if days == nil {
		days = []int{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":      q.Enabled,
		"start_minute": q.StartMinute,
		"end_minute":   q.EndMinute,
		"days_of_week": days,
		"tz":           q.TZ,
	})
}

// acctQuietHoursBody is the quiet-hours PUT body.
type acctQuietHoursBody struct {
	Enabled     bool   `json:"enabled"`
	StartMinute int    `json:"start_minute"`
	EndMinute   int    `json:"end_minute"`
	DaysOfWeek  []int  `json:"days_of_week"`
	TZ          string `json:"tz"`
}

// acctValidateQuietHours defaults TZ and returns a 400 message, or "" when valid.
func acctValidateQuietHours(body *acctQuietHoursBody) string {
	if body.StartMinute < 0 || body.StartMinute >= 1440 || body.EndMinute < 0 || body.EndMinute >= 1440 {
		return "start_minute/end_minute must be in [0,1440)"
	}
	for _, d := range body.DaysOfWeek {
		if d < 0 || d > 6 {
			return "days_of_week entries must be 0..6 (Sun..Sat)"
		}
	}
	if body.TZ == "" {
		body.TZ = "UTC"
	}
	if _, err := time.LoadLocation(body.TZ); err != nil {
		return "invalid tz (use an IANA name like Europe/Berlin)"
	}
	return ""
}

func (h *NotificationSettingsHandlers) putQuietHours(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body acctQuietHoursBody
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if msg := acctValidateQuietHours(&body); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if err := h.Settings.SetQuietHours(r.Context(), domain.QuietHours{
		UserID:      userID,
		Enabled:     body.Enabled,
		StartMinute: body.StartMinute,
		EndMinute:   body.EndMinute,
		DaysOfWeek:  body.DaysOfWeek,
		TZ:          body.TZ,
	}); err != nil {
		h.Log.Error("upsert quiet hours failed", "user", userID, "error", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *NotificationSettingsHandlers) deliveries(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 500)
	}
	rows, err := h.Settings.RecentDeliveries(r.Context(), userID, limit)
	if err != nil {
		h.Log.Error("list deliveries failed", "user", userID, "error", err)
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, d := range rows {
		var wh string
		if d.WebhookEndpoint != nil {
			wh = d.WebhookEndpoint.String()
		}
		out = append(out, map[string]any{
			"event_id":         d.EventID.String(),
			"channel":          string(d.Channel),
			"webhook_endpoint": wh,
			"email_address":    d.EmailAddress,
			"status":           string(d.Status),
			"http_status_code": d.HTTPStatusCode,
			"error":            d.ErrorMessage,
			"attempt":          d.Attempt,
			"attempted_at":     d.AttemptedAt.UTC(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

// acctWebhookJSON is the safe display shape of an endpoint (never the secret).
func acctWebhookJSON(e domain.WebhookEndpoint) map[string]any {
	types := make([]int, 0, len(e.EventTypes))
	for _, t := range e.EventTypes {
		types = append(types, int(t))
	}
	var lastAt any
	if e.LastDeliveryAt != nil {
		lastAt = e.LastDeliveryAt.UTC()
	}
	return map[string]any{
		"id":                   e.ID.String(),
		"name":                 e.Name,
		"url":                  e.URL,
		"event_types":          types,
		"min_severity":         string(e.MinSeverity),
		"enabled":              e.Enabled,
		"auto_disabled":        e.AutoDisabled,
		"last_delivery_at":     lastAt,
		"last_status_code":     e.LastDeliveryStatusCode,
		"last_error":           e.LastDeliveryError,
		"consecutive_failures": e.ConsecutiveFailures,
		"created_at":           e.CreatedAt.UTC(),
	}
}

// acctWebhookBody is the webhook create/update body.
type acctWebhookBody struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	EventTypes  []int  `json:"event_types"`
	MinSeverity string `json:"min_severity"`
	Enabled     *bool  `json:"enabled"`
}

func (b acctWebhookBody) endpoint(id domain.WebhookEndpointID, userID domain.UserID) domain.WebhookEndpoint {
	types := make([]domain.NotificationType, 0, len(b.EventTypes))
	for _, t := range b.EventTypes {
		types = append(types, domain.NotificationType(t))
	}
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	return domain.WebhookEndpoint{
		ID: id, UserID: userID, Name: b.Name, URL: b.URL, EventTypes: types,
		MinSeverity: domain.NotificationSeverity(b.MinSeverity), Enabled: enabled,
	}
}

// webhookAndID resolves the caller and the {id} path value or writes an error.
func (h *NotificationSettingsHandlers) webhookAndID(w http.ResponseWriter, r *http.Request) (domain.UserID, domain.WebhookEndpointID, bool) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return userID, domain.WebhookEndpointID{}, false
	}
	id, err := domain.ParseUUID[domain.WebhookEndpointID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return userID, id, false
	}
	return userID, id, true
}

// decodeWebhook reads and validates a create/update body or writes an error.
func (h *NotificationSettingsHandlers) decodeWebhook(w http.ResponseWriter, r *http.Request) (acctWebhookBody, bool) {
	var body acctWebhookBody
	if !requireJSON(w, r) {
		return body, false
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return body, false
	}
	body.Name = strings.TrimSpace(body.Name)
	body.URL = strings.TrimSpace(body.URL)
	if body.Name == "" || !acctIsHTTPURL(body.URL) {
		http.Error(w, "name and a valid http(s) url are required", http.StatusBadRequest)
		return body, false
	}
	if body.MinSeverity == "" {
		body.MinSeverity = string(domain.NotificationSeverityInfo)
	}
	if !domain.NotificationSeverity(body.MinSeverity).Valid() {
		http.Error(w, "invalid min_severity", http.StatusBadRequest)
		return body, false
	}
	return body, true
}

// acctIsHTTPURL is create-time feedback on a webhook URL: http(s), a host,
// no literal internal IP. The dial-time guard in the delivery client is the
// authoritative SSRF protection.
func acctIsHTTPURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && safehttp.IsBlocked(ip) {
		return false
	}
	return true
}

// writeWebhookErr maps a store error to 404/500; it reports whether one was written.
func (h *NotificationSettingsHandlers) writeWebhookErr(w http.ResponseWriter, err error, logMsg, httpMsg string, id domain.WebhookEndpointID) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, domain.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, notification.ErrWebhookSecret):
		http.Error(w, "secret generation failed", http.StatusInternalServerError)
	default:
		h.Log.Error(logMsg, "id", id, "error", err)
		http.Error(w, httpMsg, http.StatusInternalServerError)
	}
	return true
}

func (h *NotificationSettingsHandlers) listWebhooks(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	eps, err := h.Webhooks.List(r.Context(), userID)
	if err != nil {
		h.Log.Error("list webhooks failed", "user", userID, "error", err)
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(eps))
	for _, e := range eps {
		out = append(out, acctWebhookJSON(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out})
}

func (h *NotificationSettingsHandlers) createWebhook(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	body, ok := h.decodeWebhook(w, r)
	if !ok {
		return
	}
	created, secret, err := h.Webhooks.Create(r.Context(), body.endpoint(domain.WebhookEndpointID{}, userID))
	if err != nil {
		if errors.Is(err, notification.ErrWebhookSecret) {
			http.Error(w, "secret generation failed", http.StatusInternalServerError)
			return
		}
		h.Log.Error("create webhook failed", "user", userID, "error", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	resp := acctWebhookJSON(created)
	resp["secret"] = secret // shown once
	writeJSON(w, http.StatusCreated, resp)
}

func (h *NotificationSettingsHandlers) updateWebhook(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.webhookAndID(w, r)
	if !ok {
		return
	}
	body, ok := h.decodeWebhook(w, r)
	if !ok {
		return
	}
	err := h.Webhooks.Update(r.Context(), body.endpoint(id, userID))
	if h.writeWebhookErr(w, err, "update webhook failed", "update failed", id) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *NotificationSettingsHandlers) rotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.webhookAndID(w, r)
	if !ok {
		return
	}
	secret, err := h.Webhooks.RotateSecret(r.Context(), userID, id)
	if h.writeWebhookErr(w, err, "rotate webhook secret failed", "rotate failed", id) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secret": secret})
}

func (h *NotificationSettingsHandlers) testWebhook(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.webhookAndID(w, r)
	if !ok {
		return
	}
	ep, err := h.Webhooks.Test(r.Context(), userID, id)
	if h.writeWebhookErr(w, err, "get webhook failed", "load failed", id) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"delivered":        ep.LastDeliveryError == "" && ep.LastDeliveryStatusCode >= 200 && ep.LastDeliveryStatusCode < 300,
		"last_status_code": ep.LastDeliveryStatusCode,
		"last_error":       ep.LastDeliveryError,
	})
}

func (h *NotificationSettingsHandlers) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.webhookAndID(w, r)
	if !ok {
		return
	}
	err := h.Webhooks.Delete(r.Context(), userID, id)
	if h.writeWebhookErr(w, err, "delete webhook failed", "delete failed", id) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
