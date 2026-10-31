package rest

import (
	"net/http"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/instance"
	"github.com/opencairn-org/cairn-core/internal/usecase/invite"
)

// AdminInstanceHandlers are the REST twin of the instance-level admin RPCs.
// Routes whose use case is nil are not mounted, except the test email (503).
//
//	GET  /api/admin/instance/settings
//	PUT  /api/admin/instance/settings   { instance_name?, ..., allowed_email_domains?, per_user_storage_quota_bytes?, per_user_max_external_accounts? }
//	POST /api/admin/email/test          { to? }
//	GET  /api/admin/health
//	POST /api/admin/invites/{id}/resend
type AdminInstanceHandlers struct {
	Auth      *Auth
	Settings  *instance.Settings
	TestEmail *instance.TestEmail
	Health    *instance.Health
	Invites   *invite.Admin
}

// Mount registers the routes.
func (h *AdminInstanceHandlers) Mount(mux *http.ServeMux) {
	if h.Settings != nil {
		mux.HandleFunc("GET /api/admin/instance/settings", h.getSettings)
		mux.HandleFunc("PUT /api/admin/instance/settings", h.putSettings)
	}
	mux.HandleFunc("POST /api/admin/email/test", h.testEmail)
	if h.Health != nil {
		mux.HandleFunc("GET /api/admin/health", h.health)
	}
	if h.Invites != nil {
		mux.HandleFunc("POST /api/admin/invites/{id}/resend", h.resendInvite)
	}
}

func (h *AdminInstanceHandlers) getSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminGate(h.Auth, w, r); !ok {
		return
	}
	cur, err := h.Settings.Get(r.Context())
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": adminInstanceSettingsJSON(cur)})
}

type adminInstanceSettingsBody struct {
	Name                       *string   `json:"instance_name"`
	Description                *string   `json:"instance_description"`
	SupportEmail               *string   `json:"support_email"`
	URL                        *string   `json:"instance_url"`
	LogoURL                    *string   `json:"logo_url"`
	PrimaryColor               *string   `json:"primary_color"`
	DefaultLocale              *string   `json:"default_locale"`
	DefaultTimezone            *string   `json:"default_timezone"`
	DefaultUnits               *string   `json:"default_units"`
	RegistrationOpen           *bool     `json:"registration_open"`
	RequireInvite              *bool     `json:"require_invite"`
	AllowedEmailDomains        *[]string `json:"allowed_email_domains"`
	PerUserStorageQuotaBytes   **int64   `json:"per_user_storage_quota_bytes"`
	PerUserMaxExternalAccounts **int     `json:"per_user_max_external_accounts"`
}

func (h *AdminInstanceHandlers) putSettings(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok || !requireJSON(w, r) {
		return
	}
	var b adminInstanceSettingsBody
	if err := decodeJSONBody(r, &b); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	p := domain.InstanceSettingsPatch{
		Name: b.Name, Description: b.Description, SupportEmail: b.SupportEmail, URL: b.URL, LogoURL: b.LogoURL, PrimaryColor: b.PrimaryColor,
		DefaultLocale: b.DefaultLocale, DefaultTimezone: b.DefaultTimezone, RegistrationOpen: b.RegistrationOpen, RequireInvite: b.RequireInvite,
		AllowedEmailDomains: b.AllowedEmailDomains, PerUserStorageQuotaBytes: b.PerUserStorageQuotaBytes, PerUserMaxExternalAccounts: b.PerUserMaxExternalAccounts,
	}
	if b.DefaultUnits != nil {
		u := domain.UserUnits(*b.DefaultUnits)
		p.DefaultUnits = &u
	}
	cur, err := h.Settings.Update(r.Context(), actor(r, admin), p)
	if err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": adminInstanceSettingsJSON(cur)})
}

func (h *AdminInstanceHandlers) testEmail(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok {
		return
	}
	if h.TestEmail == nil {
		http.Error(w, "no email adapter is configured", http.StatusServiceUnavailable)
		return
	}
	var b struct {
		To string `json:"to"`
	}
	_ = decodeOptionalJSONBody(r, &b)
	if b.To == "" {
		b.To = admin.Email
	}
	took, err := h.TestEmail.Send(r.Context(), b.To)
	if err != nil {
		if _, isAction := actionErrorOf(err); isAction {
			writeActionError(w, err, "send failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"delivered": false, "error": err.Error(), "duration_ms": took.Milliseconds()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"delivered": true, "duration_ms": took.Milliseconds()})
}

func (h *AdminInstanceHandlers) health(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminGate(h.Auth, w, r); !ok {
		return
	}
	rep := h.Health.Check(r.Context())
	comp := func(c instance.Component) map[string]any {
		return map[string]any{"state": string(c.State), "detail": c.Detail, "measurements": c.Measurements}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"overall": string(rep.Overall), "database": comp(rep.Database), "queue": comp(rep.Queue), "storage": comp(rep.Storage), "email": comp(rep.Email),
		"workers":    map[string]int{"healthy": rep.Workers.Healthy, "degraded": rep.Workers.Degraded, "offline": rep.Workers.Offline},
		"jobs":       map[string]int{"queued": rep.JobsQueued, "running": rep.JobsRunning, "dead_letter": rep.JobsDLQ},
		"checked_at": rep.CheckedAt.UTC().Format(time.RFC3339),
	})
}

func (h *AdminInstanceHandlers) resendInvite(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminGate(h.Auth, w, r)
	if !ok {
		return
	}
	id, ok := adminParseID[domain.InviteID](w, r)
	if !ok {
		return
	}
	res, err := h.Invites.Resend(r.Context(), actor(r, admin), id)
	if err != nil {
		writeActionError(w, err, "resend failed")
		return
	}
	out := map[string]any{"delivered": res.Delivered, "prefix": res.Invite.CodePrefix}
	if res.Err != nil {
		out["error"] = res.Err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

func adminInstanceSettingsJSON(s domain.InstanceSettings) map[string]any {
	return map[string]any{
		"instance_name": s.Name, "instance_description": s.Description, "support_email": s.SupportEmail, "instance_url": s.URL,
		"logo_url": s.LogoURL, "primary_color": s.PrimaryColor, "default_locale": s.DefaultLocale, "default_timezone": s.DefaultTimezone,
		"default_units": string(s.DefaultUnits), "registration_open": s.RegistrationOpen, "require_invite": s.RequireInvite,
		"allowed_email_domains": s.AllowedEmailDomains, "email_adapter": s.EmailAdapter, "email_configured": s.EmailConfigured,
		"email_from_address": s.EmailFromAddress, "storage_adapter": s.StorageAdapter, "storage_configured": s.StorageConfigured,
		"per_user_storage_quota_bytes": s.PerUserStorageQuotaBytes, "per_user_max_external_accounts": s.PerUserMaxExternalAccounts,
		"updated_at": s.UpdatedAt.UTC().Format(time.RFC3339),
	}
}
