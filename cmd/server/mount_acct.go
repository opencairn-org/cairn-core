package main

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
	"github.com/opencairn-org/cairn-core/internal/usecase/notification"
)

// acctSessions issues and ends cookie sessions; nil without a session store.
func acctSessions(app *App, ttl time.Duration) *authuc.Sessions {
	if app.Sessions == nil {
		return nil
	}
	return &authuc.Sessions{Repo: app.Sessions, TTL: ttl}
}

// acctVerification mails and confirms email-verification links.
func acctVerification(app *App, base string) *account.EmailVerification {
	base = strings.TrimRight(base, "/")
	return &account.EmailVerification{
		Users: app.Users, Tokens: app.AuthTokens, Mail: app.Email,
		Link: func(code string) string { return base + "/auth/email/verify?code=" + code },
	}
}

func mountAcctOIDC(mux *http.ServeMux, app *App, ttl time.Duration, logger *slog.Logger) {
	h := &rest.OIDCHandlers{OIDC: app.OIDCLogin, Sessions: acctSessions(app, ttl), SecureCookies: secureCookiesAlways, Log: logger}
	if app.SecretBox != nil {
		h.FlowBox = app.SecretBox
	}
	h.Mount(mux)
	if app.OIDCLogin == nil {
		logger.Info("oidc endpoints not mounted: no providers configured (logout still mounted)")
		return
	}
	logger.Info("oidc endpoints mounted", "paths", []string{"/auth/oidc/clients", "/auth/oidc/{provider}/start", "/auth/oidc/callback"})
}

func mountAcctPasswordAuth(mux *http.ServeMux, app *App, logger *slog.Logger, publicURL string, ttl time.Duration) {
	(&rest.PasswordAuthHandlers{
		Auth:           restAuth(app),
		Login:          &authuc.PasswordLogin{Users: app.Users, Hasher: app.PasswordHasher, Guard: app.LoginGuard},
		Sessions:       acctSessions(app, ttl),
		SecondFactor:   app.SecondFactor,
		PasswordChange: app.PasswordChange,
		Passwords:      app.Passwords,
		Verification:   acctVerification(app, publicURL),
		MailEnabled:    app.Email != nil,
		BaseURL:        strings.TrimRight(publicURL, "/"),
		SecureCookies:  secureCookiesAlways,
		Log:            logger,
	}).Mount(mux)
	logger.Info("password auth endpoints mounted (sign-in, password reset, email verification)")
}

func mountAcctWebAuthn(mux *http.ServeMux, app *App, logger *slog.Logger, publicURL string, ttl time.Duration) {
	if app.Passkeys == nil || app.Users == nil || app.Sessions == nil {
		return
	}
	wa, err := rest.NewWebAuthn(publicURL)
	if err != nil {
		logger.Error("webauthn disabled: bad config", "error", err)
		return
	}
	(&rest.WebAuthnHandlers{
		Auth:          restAuth(app),
		WebAuthn:      wa,
		Passkeys:      &account.Passkeys{Repo: app.Passkeys, Users: app.Users},
		Sessions:      acctSessions(app, ttl),
		SecondFactor:  app.SecondFactor,
		SecureCookies: secureCookiesAlways,
		Log:           logger,
	}).Mount(mux)
	logger.Info("webauthn endpoints mounted", "rpid", wa.Config.RPID, "origins", wa.Config.RPOrigins)
}

func mountAcctInvites(mux *http.ServeMux, app *App, logger *slog.Logger, publicURL string, ttl time.Duration) {
	if app.CreateInvite == nil || app.RedeemInvite == nil || app.Invites == nil || app.InviteAdmin == nil {
		return
	}
	(&rest.InviteHandlers{
		Auth:          restAuth(app),
		Admin:         app.InviteAdmin,
		Redeem:        app.RedeemInvite,
		Sessions:      acctSessions(app, ttl),
		Verification:  acctVerification(app, publicURL),
		BaseURL:       strings.TrimRight(publicURL, "/"),
		SecureCookies: secureCookiesAlways,
		Log:           logger,
	}).Mount(mux)
	logger.Info("invite endpoints mounted")
}

// mountAcctAccount mounts password change, sessions, identities, profile, authorized apps and PATs.
func mountAcctAccount(mux *http.ServeMux, app *App, logger *slog.Logger, ttl time.Duration) {
	(&rest.AccountHandlers{
		Auth:           restAuth(app),
		Sessions:       acctSessions(app, ttl),
		Passwords:      app.Passwords,
		SessionMgmt:    app.SessionMgmt,
		Identities:     app.Identities,
		Profiles:       app.Profiles,
		AuthorizedApps: app.AuthorizedApps,
		Tokens:         app.AccountTokens,
	}).Mount(mux)
	logger.Info("account endpoints mounted")
}

func mountAcctLifecycle(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.ExternalAccounts == nil {
		return
	}
	(&rest.AccountLifecycleHandlers{
		Auth: restAuth(app), Accounts: app.Accounts, Remover: app.AccountRemover, Tokens: app.WebhookTokens,
	}).Mount(mux)
	logger.Info("account lifecycle endpoints mounted")
}

func mountAcctNotifications(mux *http.ServeMux, app *App, logger *slog.Logger) {
	// Only set the ports that exist: a nil repo in an interface would read as present.
	settings := &notification.Settings{Mail: app.Email}
	if app.Users != nil {
		settings.Users = app.Users
	}
	if app.QuietHours != nil {
		settings.QuietHours = app.QuietHours
	}
	if app.NotificationDeliveries != nil {
		settings.Deliveries = app.NotificationDeliveries
	}
	h := &rest.NotificationSettingsHandlers{
		Auth: restAuth(app), Preferences: app.NotificationPreferences, Settings: settings, Log: logger,
	}
	if app.WebhookEndpoints != nil {
		h.Webhooks = &notification.Webhooks{Repo: app.WebhookEndpoints, Deliver: app.DeliverNotifications}
	}
	h.Mount(mux)
	logger.Info("notification settings endpoints mounted")
}
