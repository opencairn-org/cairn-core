package providerlink

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// needsReauthSubject carries the event user-facing notifiers react to.
const needsReauthSubject = "cairn.events.external_account.needs_reauth"

// Credential lookup failures a worker maps to its own retry policy.
var (
	ErrAccountGone = errors.New("account gone")
	ErrNeedsReauth = errors.New("credentials need re-entry")
)

// WorkerTokens is the provider-agnostic token store workers read and write.
type WorkerTokens struct {
	Tokens port.OAuthTokenRepo
	// Accounts and Configs resolve the per-user app credentials; either may
	// be nil, the token then comes without them.
	Accounts port.ExternalAccountRepo
	Configs  port.UserProviderConfigRepo
	Bus      port.JobBus // publishes needs_reauth; nil skips the event
	Log      *slog.Logger
	Now      func() time.Time
}

// AccountToken is a stored token plus the owning connection's app
// credentials, so the worker can refresh against the provider.
type AccountToken struct {
	State        port.TokenState
	ClientID     string
	ClientSecret string
}

// Get returns the account's token; port.ErrTokenAccountNotFound when the
// account is gone or needs re-auth.
func (t *WorkerTokens) Get(ctx context.Context, id domain.ExternalAccountID) (AccountToken, error) {
	st, err := t.Tokens.GetToken(ctx, id)
	if err != nil {
		return AccountToken{}, err
	}
	out := AccountToken{State: st}
	// Best-effort: without the app credentials the worker still has the
	// access token until expiry and surfaces needs_reauth on refresh.
	if conn, err := t.connection(ctx, id); err == nil {
		out.ClientID, out.ClientSecret = conn.ClientID, conn.ClientSecret
	} else {
		t.Log.Debug("token get: no app credentials", "account_id", id, "error", err)
	}
	return out, nil
}

// connection loads the connection behind an account.
func (t *WorkerTokens) connection(ctx context.Context, id domain.ExternalAccountID) (domain.UserProviderConfig, error) {
	if t.Accounts == nil || t.Configs == nil {
		return domain.UserProviderConfig{}, errors.New("accounts not wired")
	}
	acct, err := t.Accounts.GetExternalAccount(ctx, id)
	if err != nil {
		return domain.UserProviderConfig{}, err
	}
	if acct.ConnectionID == nil {
		return domain.UserProviderConfig{}, errors.New("account has no connection")
	}
	return t.Configs.GetByID(ctx, *acct.ConnectionID)
}

// Store replaces the account's token state (optimistic on the previous
// expiry, port.ErrTokenStaleStore when another refresh won).
func (t *WorkerTokens) Store(ctx context.Context, id domain.ExternalAccountID, in port.StoreTokenInput) error {
	return t.Tokens.StoreToken(ctx, id, in)
}

// MarkNeedsReauth flags the account and announces it. The event is
// best-effort: a failed publish does not undo the flag.
func (t *WorkerTokens) MarkNeedsReauth(ctx context.Context, id domain.ExternalAccountID, reason string) error {
	if reason == "" {
		reason = "refresh_failed"
	}
	if err := t.Tokens.MarkNeedsReauth(ctx, id, reason); err != nil {
		return err
	}
	if t.Bus == nil {
		return nil
	}
	now := time.Now().UTC()
	if t.Now != nil {
		now = t.Now()
	}
	body, _ := json.Marshal(map[string]any{
		"account_id": id.String(),
		"reason":     reason,
		"at":         now.Format(time.RFC3339),
	})
	if err := t.Bus.Publish(ctx, needsReauthSubject, "reauth:"+id.String(), body); err != nil {
		t.Log.Warn("publish needs_reauth event failed", "account_id", id, "error", err)
	}
	t.Log.Info("account marked needs_reauth", "account_id", id, "reason", reason)
	return nil
}

// Login returns a credential provider's stored login (email/password):
// ErrAccountGone, ErrNeedsReauth, or a transient error.
func (t *WorkerTokens) Login(ctx context.Context, id domain.ExternalAccountID) (string, string, error) {
	acct, err := t.Accounts.GetExternalAccount(ctx, id)
	if err != nil || acct.ConnectionID == nil {
		return "", "", ErrAccountGone
	}
	cfg, err := t.Configs.GetByID(ctx, *acct.ConnectionID)
	if err != nil {
		return "", "", err
	}
	if cfg.SecretUnreadable || cfg.ClientSecret == "" {
		return "", "", ErrNeedsReauth
	}
	return cfg.ClientID, cfg.ClientSecret, nil
}
