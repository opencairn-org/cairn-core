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
	// ErrWrongProvider: the account exists but belongs to another provider
	// than the subject the worker asked on. Subject permissions scope a
	// worker to its provider; this check keeps that scope on the body too.
	ErrWrongProvider = errors.New("account belongs to another provider")
)

// WorkerTokens is the provider-agnostic token store workers read and write.
// Every call names the provider of the subject the worker asked on, and the
// account must belong to it.
type WorkerTokens struct {
	Tokens   port.OAuthTokenRepo
	Accounts port.ExternalAccountRepo
	// Configs resolves the per-user app credentials; nil leaves them out.
	Configs port.UserProviderConfigRepo
	Bus     port.JobBus // publishes needs_reauth; nil skips the event
	Log     *slog.Logger
	Now     func() time.Time
}

// AccountToken is a stored token plus the owning connection's app
// credentials, so the worker can refresh against the provider.
type AccountToken struct {
	State        port.TokenState
	ClientID     string
	ClientSecret string
}

// Get returns the account's token; port.ErrTokenAccountNotFound when the
// account is gone or needs re-auth, ErrWrongProvider on a provider mismatch.
func (t *WorkerTokens) Get(ctx context.Context, provider string, id domain.ExternalAccountID) (AccountToken, error) {
	acct, err := t.account(ctx, provider, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return AccountToken{}, port.ErrTokenAccountNotFound
		}
		return AccountToken{}, err
	}
	st, err := t.Tokens.GetToken(ctx, id)
	if err != nil {
		return AccountToken{}, err
	}
	out := AccountToken{State: st}
	// Best-effort: without the app credentials the worker still has the
	// access token until expiry and surfaces needs_reauth on refresh.
	if conn, err := t.connection(ctx, acct); err == nil {
		out.ClientID, out.ClientSecret = conn.ClientID, conn.ClientSecret
	} else {
		t.Log.Debug("token get: no app credentials", "account_id", id, "error", err)
	}
	return out, nil
}

// account loads the account and checks it belongs to the provider.
func (t *WorkerTokens) account(ctx context.Context, provider string, id domain.ExternalAccountID) (domain.ExternalAccount, error) {
	if t.Accounts == nil {
		return domain.ExternalAccount{}, errors.New("accounts not wired")
	}
	acct, err := t.Accounts.GetExternalAccount(ctx, id)
	if err != nil {
		return domain.ExternalAccount{}, err
	}
	if acct.Provider != provider {
		return domain.ExternalAccount{}, ErrWrongProvider
	}
	return acct, nil
}

// connection loads the connection behind an account.
func (t *WorkerTokens) connection(ctx context.Context, acct domain.ExternalAccount) (domain.UserProviderConfig, error) {
	if t.Configs == nil {
		return domain.UserProviderConfig{}, errors.New("configs not wired")
	}
	if acct.ConnectionID == nil {
		return domain.UserProviderConfig{}, errors.New("account has no connection")
	}
	return t.Configs.GetByID(ctx, *acct.ConnectionID)
}

// Store replaces the account's token state (optimistic on the previous
// expiry, port.ErrTokenStaleStore when another refresh won).
func (t *WorkerTokens) Store(ctx context.Context, provider string, id domain.ExternalAccountID, in port.StoreTokenInput) error {
	if _, err := t.account(ctx, provider, id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return port.ErrTokenAccountNotFound
		}
		return err
	}
	return t.Tokens.StoreToken(ctx, id, in)
}

// MarkNeedsReauth flags the account and announces it. The event is
// best-effort: a failed publish does not undo the flag.
func (t *WorkerTokens) MarkNeedsReauth(ctx context.Context, provider string, id domain.ExternalAccountID, reason string) error {
	if _, err := t.account(ctx, provider, id); err != nil {
		return err
	}
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

// Login returns a credential provider's stored login (the connection's
// client_id/client_secret pair): ErrAccountGone, ErrNeedsReauth,
// ErrWrongProvider, or a transient error.
func (t *WorkerTokens) Login(ctx context.Context, provider string, id domain.ExternalAccountID) (string, string, error) {
	acct, err := t.account(ctx, provider, id)
	if err != nil {
		if errors.Is(err, ErrWrongProvider) {
			return "", "", err
		}
		return "", "", ErrAccountGone
	}
	if acct.ConnectionID == nil {
		return "", "", ErrAccountGone
	}
	cfg, err := t.connection(ctx, acct)
	if err != nil {
		return "", "", err
	}
	if cfg.SecretUnreadable || cfg.ClientSecret == "" {
		return "", "", ErrNeedsReauth
	}
	return cfg.ClientID, cfg.ClientSecret, nil
}
