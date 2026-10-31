package providerlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ConnectError is a failed connect round-trip step; its value is the reason
// code the connections page shows (?error=<code>).
type ConnectError string

func (e ConnectError) Error() string { return "connect: " + string(e) }

// Connect round-trip failures.
const (
	ErrNoConnection     ConnectError = "no_connection"
	ErrConfigureFirst   ConnectError = "configure_first"
	ErrSecretUnreadable ConnectError = "secret_unreadable"
	ErrBadWorker        ConnectError = "bad_worker"
	ErrExchangeFailed   ConnectError = "exchange_failed"
	ErrChooseWorker     ConnectError = "choose_worker"
)

var (
	// ErrUnknownProvider: no such provider.
	ErrUnknownProvider = errors.New("unknown provider")
	// ErrNotOAuth: the provider links by credentials, not an OAuth round-trip.
	ErrNotOAuth = errors.New("provider does not use OAuth")
)

// Connections manages a user's provider connections and links their accounts.
type Connections struct {
	Providers map[string]Provider
	Configs   port.UserProviderConfigRepo
	// Accounts is nil when external accounts are not wired; credential
	// connections are then stored without an account.
	Accounts  port.ExternalAccountRepo
	Tokens    port.OAuthTokenRepo
	Pools     port.PoolDirectory
	Exchanger port.ProviderCodeExchanger
	// Bus reads worker presence for the webhook endpoints; may be nil.
	Bus  port.JobBus
	Jobs *Jobs // first gear sync of a new account; may be nil
	Log  *slog.Logger
}

// NewConnection is a connection to create.
type NewConnection struct {
	Provider     string
	Label        string
	ClientID     string
	ClientSecret string
	// Worker is the pool the account is connected for (docs/adr/0006);
	// required only when several pools serve the provider.
	Worker string
}

func invalid(msg string) error { return port.NewActionError(port.ActionErrorInvalid, msg) }

// Create stores a connection. A credential provider has no authorize
// round-trip, so its account is created with it.
func (c *Connections) Create(ctx context.Context, userID domain.UserID, in NewConnection) (domain.ConnectionID, error) {
	p, ok := c.Providers[in.Provider]
	if !ok {
		return domain.ConnectionID{}, invalid("unknown provider")
	}
	login := strings.TrimSpace(in.ClientID)
	if login == "" {
		return domain.ConnectionID{}, invalid("client_id is required")
	}
	// Credential providers: without the secret the connection could never
	// authenticate.
	if !p.OAuth() && strings.TrimSpace(in.ClientSecret) == "" {
		return domain.ConnectionID{}, invalid("client_secret is required")
	}
	id, err := c.Configs.Create(ctx, domain.UserProviderConfig{
		UserID: userID, Provider: in.Provider, Label: strings.TrimSpace(in.Label),
		ClientID: login, ClientSecret: strings.TrimSpace(in.ClientSecret),
	})
	if err != nil {
		c.Log.Error("create connection failed", "error", err)
		return domain.ConnectionID{}, fmt.Errorf("create connection: %w", err)
	}
	if !p.OAuth() && c.Accounts != nil {
		if err := c.linkCredentialAccount(ctx, userID, id, in.Provider, login, in.Worker); err != nil {
			if errors.Is(err, domain.ErrWorkerAmbiguous) || errors.Is(err, domain.ErrNoWorker) {
				return domain.ConnectionID{}, invalid(err.Error())
			}
			return domain.ConnectionID{}, err
		}
	}
	return id, nil
}

// linkCredentialAccount creates a credential provider's account; on failure
// it rolls the connection back so the user can retry cleanly.
func (c *Connections) linkCredentialAccount(ctx context.Context, userID domain.UserID, id domain.ConnectionID, provider, login, worker string) error {
	pool, err := c.pool(ctx, userID, provider, id, login, worker)
	var acctID domain.ExternalAccountID
	if err == nil {
		acctID, err = c.Accounts.CreateAccount(ctx, domain.ExternalAccount{
			UserID: userID, Provider: provider, ProviderAccountID: login, ConnectionID: &id,
			DisplayLabel: login, AssignedWorkerName: pool,
			Status: domain.ExternalAccountStatusActive, AutoImportEnabled: true,
		})
	}
	if err != nil {
		_ = c.Configs.Delete(ctx, id)
		c.Log.Error("create connection: account link failed", "provider", provider, "error", err)
		return err
	}
	c.Log.Info("connection + account created", "provider", provider, "account", acctID, "worker", pool)
	c.Jobs.SyncGearOnConnect(ctx, acctID)
	return nil
}

// pool resolves the worker pool a connection's account is created for
// (domain.ResolveConnectPool): the user's choice, else the pool this
// connection already used for the same provider account, else the only one.
func (c *Connections) pool(ctx context.Context, userID domain.UserID, provider string, conn domain.ConnectionID, providerAcctID, requested string) (string, error) {
	var existing string
	if accts, err := c.Accounts.ListAccountsForUser(ctx, userID); err == nil {
		for _, a := range accts {
			if a.Provider == provider && a.ProviderAccountID == providerAcctID && a.ConnectionID != nil && *a.ConnectionID == conn {
				existing = a.AssignedWorkerName
			}
		}
	}
	var known []string
	if c.Pools != nil {
		known = c.Pools.KnownPoolNames(ctx, provider)
	}
	return domain.ResolveConnectPool(requested, existing, known)
}

// Own loads the caller's connection; domain.ErrNotFound for anyone else's.
func (c *Connections) Own(ctx context.Context, userID domain.UserID, id domain.ConnectionID) (domain.UserProviderConfig, error) {
	conn, err := c.Configs.GetByID(ctx, id)
	if err != nil || conn.UserID != userID {
		return domain.UserProviderConfig{}, domain.ErrNotFound
	}
	return conn, nil
}

// Update changes an owned connection's label and credentials; an empty
// clientID or secret keeps the stored one.
func (c *Connections) Update(ctx context.Context, conn domain.UserProviderConfig, label, clientID, secret string) error {
	conn.Label = strings.TrimSpace(label)
	if id := strings.TrimSpace(clientID); id != "" {
		conn.ClientID = id
	}
	conn.ClientSecret = strings.TrimSpace(secret)
	return c.Configs.Update(ctx, conn)
}

// Delete removes an owned connection.
func (c *Connections) Delete(ctx context.Context, conn domain.UserProviderConfig) error {
	return c.Configs.Delete(ctx, conn.ID)
}

// OAuthProvider resolves a provider that links via the OAuth round-trip.
func (c *Connections) OAuthProvider(id string) (Provider, error) {
	p, ok := c.Providers[id]
	if !ok {
		return Provider{}, ErrUnknownProvider
	}
	if !p.OAuth() {
		return Provider{}, ErrNotOAuth
	}
	return p, nil
}

// AuthorizeURL checks that the caller's connection can start the round-trip
// and returns the provider's authorize URL.
func (c *Connections) AuthorizeURL(ctx context.Context, userID domain.UserID, p Provider, connID domain.ConnectionID, worker, state, redirectURI string) (string, error) {
	conn, err := c.Configs.GetByID(ctx, connID)
	if err != nil || conn.UserID != userID || conn.Provider != p.Provider || conn.ClientID == "" {
		return "", ErrConfigureFirst
	}
	// Stored but unreadable (AAD/key change): the token exchange would fail
	// only after the provider round-trip.
	if conn.SecretUnreadable {
		return "", ErrSecretUnreadable
	}
	if worker != "" && !domain.ValidWorkerName(worker) {
		return "", ErrBadWorker
	}
	q := url.Values{}
	q.Set("client_id", conn.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", p.Scopes)
	// force: always show the provider's auth screen, so a user can pick a
	// different account for a separate connection.
	q.Set("approval_prompt", "force")
	q.Set("state", state)
	return p.AuthorizeURL + "?" + q.Encode(), nil
}

// Complete exchanges the callback's code and links the account to the
// caller's connection; it returns the new account.
func (c *Connections) Complete(ctx context.Context, userID domain.UserID, p Provider, connID domain.ConnectionID, code, requestedWorker string) (domain.ExternalAccountID, error) {
	conn, err := c.Configs.GetByID(ctx, connID)
	if err != nil || conn.UserID != userID {
		return domain.ExternalAccountID{}, ErrConfigureFirst
	}
	tok, providerAcctID, err := c.Exchanger.ExchangeCode(ctx, p.TokenURL, conn, code)
	if err != nil {
		c.Log.Error("oauth code exchange failed", "provider", p.Provider, "error", err)
		return domain.ExternalAccountID{}, ErrExchangeFailed
	}
	label := p.DisplayName
	if conn.Label != "" {
		label = conn.Label
	}
	pool, err := c.pool(ctx, userID, p.Provider, conn.ID, providerAcctID, requestedWorker)
	if err != nil {
		c.Log.Warn("oauth connect: no worker pool", "provider", p.Provider, "error", err)
		return domain.ExternalAccountID{}, ErrChooseWorker
	}
	acctID, err := c.Accounts.CreateAccount(ctx, domain.ExternalAccount{
		UserID: userID, Provider: p.Provider, ProviderAccountID: providerAcctID,
		ConnectionID: &conn.ID, DisplayLabel: label, GrantedScopes: p.ScopeList(),
		AssignedWorkerName: pool,
	})
	if err != nil {
		c.Log.Error("create external account failed", "error", err)
		return domain.ExternalAccountID{}, port.NewActionError(port.ActionErrorInternal, "could not save account")
	}
	if err := c.Tokens.StoreToken(ctx, acctID, port.StoreTokenInput{State: tok}); err != nil {
		c.Log.Error("store oauth token failed", "error", err)
		return domain.ExternalAccountID{}, port.NewActionError(port.ActionErrorInternal, "could not save token")
	}
	c.Log.Info("oauth account connected", "provider", p.Provider, "account_id", acctID, "connection_id", conn.ID)
	c.Jobs.SyncGearOnConnect(ctx, acctID)
	return acctID, nil
}

// WebhookEndpoint is one advertised webhook callback.
type WebhookEndpoint struct {
	Provider  string
	WorkerKey string
	URL       string
}

// WebhookEndpoints lists the callback URLs to register with a provider's app,
// one per webhook-advertising worker ({name}-{provider}).
func (c *Connections) WebhookEndpoints(ctx context.Context, publicURL string) []WebhookEndpoint {
	out := []WebhookEndpoint{}
	if c.Bus == nil {
		return out
	}
	kv, err := c.Bus.KV("cairn_worker_presence")
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	keys, _ := kv.Keys(ctx)
	for _, k := range keys {
		entry, err := kv.Get(ctx, k)
		if err != nil {
			// Presence keys carry a TTL; one can expire between Keys and Get.
			c.Log.Debug("webhook endpoints: presence key vanished", "key", k, "error", err)
			continue
		}
		var hb struct {
			WorkerKey string `json:"worker_key"`
			Provider  string `json:"provider"`
			Webhooks  bool   `json:"webhooks"`
		}
		if json.Unmarshal(entry.Value, &hb) != nil || !hb.Webhooks || hb.WorkerKey == "" || seen[hb.WorkerKey] {
			continue
		}
		seen[hb.WorkerKey] = true
		out = append(out, WebhookEndpoint{
			Provider: hb.Provider, WorkerKey: hb.WorkerKey,
			URL: strings.TrimRight(publicURL, "/") + "/webhooks/" + hb.WorkerKey,
		})
	}
	return out
}
