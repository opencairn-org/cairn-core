// Package providerlink links provider accounts (Strava, Garmin, ...) to
// users: connection CRUD, the OAuth connect round-trip, credential-based
// accounts, the account-scoped jobs (gear sync, health import) and the
// token/credential lookups provider workers make. Provider knowledge comes
// from the workers' manifests (port.ProviderRegistry); core compiles none in.
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
	// ErrNoWorker: no pool of the provider is known, so nobody can exchange
	// the code.
	ErrNoWorker ConnectError = "no_worker"
	// ErrAlreadyLinked: another user holds this provider account; webhooks
	// carry only the athlete, so two holders could not be told apart.
	ErrAlreadyLinked ConnectError = "already_linked"
)

var (
	// ErrUnknownProvider: no such provider.
	ErrUnknownProvider = errors.New("unknown provider")
	// ErrNotOAuth: the provider links by credentials, not an OAuth round-trip.
	ErrNotOAuth = errors.New("provider does not use OAuth")
)

// Connections manages a user's provider connections and links their accounts.
type Connections struct {
	Providers port.ProviderRegistry
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

// provider loads a provider's meta; ErrUnknownProvider when none.
func (c *Connections) provider(ctx context.Context, id string) (domain.ProviderMeta, error) {
	if c.Providers == nil {
		return domain.ProviderMeta{}, ErrUnknownProvider
	}
	p, err := c.Providers.Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ProviderMeta{}, ErrUnknownProvider
	}
	return p, err
}

// Create stores a connection. A credential provider has no authorize
// round-trip, so its account is created with it.
func (c *Connections) Create(ctx context.Context, userID domain.UserID, in NewConnection) (domain.ConnectionID, error) {
	p, err := c.provider(ctx, in.Provider)
	if errors.Is(err, ErrUnknownProvider) {
		return domain.ConnectionID{}, invalid("unknown provider")
	}
	if err != nil {
		return domain.ConnectionID{}, fmt.Errorf("load provider: %w", err)
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
	for _, a := range c.connectionAccounts(ctx, userID, provider, conn) {
		if a.ProviderAccountID == providerAcctID {
			existing = a.AssignedWorkerName
		}
	}
	return domain.ResolveConnectPool(requested, existing, c.knownPools(ctx, provider))
}

func (c *Connections) knownPools(ctx context.Context, provider string) []string {
	if c.Pools == nil {
		return nil
	}
	return c.Pools.KnownPoolNames(ctx, provider)
}

// connectionAccounts lists the accounts a connection already links.
func (c *Connections) connectionAccounts(ctx context.Context, userID domain.UserID, provider string, conn domain.ConnectionID) []domain.ExternalAccount {
	if c.Accounts == nil {
		return nil
	}
	accts, err := c.Accounts.ListAccountsForUser(ctx, userID)
	if err != nil {
		return nil
	}
	var out []domain.ExternalAccount
	for _, a := range accts {
		if a.Provider == provider && a.ConnectionID != nil && *a.ConnectionID == conn {
			out = append(out, a)
		}
	}
	return out
}

// exchangePool picks the pool that exchanges the code: the requested one,
// else the only known one, else the pool this connection already uses; with
// several and no choice the user has to pick.
func (c *Connections) exchangePool(ctx context.Context, userID domain.UserID, provider string, conn domain.ConnectionID, requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	known := c.knownPools(ctx, provider)
	switch len(known) {
	case 0:
		return "", ErrNoWorker
	case 1:
		return known[0], nil
	}
	for _, a := range c.connectionAccounts(ctx, userID, provider, conn) {
		if a.AssignedWorkerName != "" {
			return a.AssignedWorkerName, nil
		}
	}
	return "", ErrChooseWorker
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
func (c *Connections) OAuthProvider(ctx context.Context, id string) (domain.ProviderMeta, error) {
	p, err := c.provider(ctx, id)
	if err != nil {
		return domain.ProviderMeta{}, err
	}
	if !p.OAuth() {
		return domain.ProviderMeta{}, ErrNotOAuth
	}
	return p, nil
}

// AuthorizeURL checks that the caller's connection can start the round-trip
// and returns the provider's authorize URL plus the PKCE verifier to keep
// for the callback ("" when the provider does not use PKCE).
func (c *Connections) AuthorizeURL(ctx context.Context, userID domain.UserID, p domain.ProviderMeta, connID domain.ConnectionID, worker, state, redirectURI string) (target, verifier string, err error) {
	conn, err := c.Configs.GetByID(ctx, connID)
	if err != nil || conn.UserID != userID || conn.Provider != p.ID || conn.ClientID == "" {
		return "", "", ErrConfigureFirst
	}
	// Stored but unreadable (AAD/key change): the token exchange would fail
	// only after the provider round-trip.
	if conn.SecretUnreadable {
		return "", "", ErrSecretUnreadable
	}
	if worker != "" && !domain.ValidWorkerName(worker) {
		return "", "", ErrBadWorker
	}
	if p.OAuth2 == nil {
		return "", "", ErrNotOAuth
	}
	q := url.Values{}
	for k, v := range p.OAuth2.AuthorizeParams {
		q.Set(k, v)
	}
	q.Set("client_id", conn.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	if s := p.ScopeString(); s != "" {
		q.Set("scope", s)
	}
	q.Set("state", state)
	if p.OAuth2.PKCE {
		var challenge string
		verifier, challenge = pkcePair()
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
	}
	sep := "?"
	if strings.Contains(p.OAuth2.AuthorizeURL, "?") {
		sep = "&"
	}
	return p.OAuth2.AuthorizeURL + sep + q.Encode(), verifier, nil
}

// Complete has the provider's pool exchange the callback's code and links
// the account to the caller's connection; it returns the new account.
func (c *Connections) Complete(ctx context.Context, userID domain.UserID, p domain.ProviderMeta, connID domain.ConnectionID, in port.CodeExchange, requestedWorker string) (domain.ExternalAccountID, error) {
	conn, err := c.Configs.GetByID(ctx, connID)
	if err != nil || conn.UserID != userID {
		return domain.ExternalAccountID{}, ErrConfigureFirst
	}
	if requestedWorker != "" && !domain.ValidWorkerName(requestedWorker) {
		return domain.ExternalAccountID{}, ErrBadWorker
	}
	exchanger, err := c.exchangePool(ctx, userID, p.ID, conn.ID, requestedWorker)
	if err != nil {
		c.Log.Warn("oauth connect: no pool to exchange the code", "provider", p.ID, "error", err)
		return domain.ExternalAccountID{}, err
	}
	if c.Exchanger == nil {
		return domain.ExternalAccountID{}, ErrExchangeFailed
	}
	tok, providerAcctID, err := c.Exchanger.ExchangeCode(ctx, p.ID, exchanger, conn, in)
	if err != nil {
		c.Log.Error("oauth code exchange failed", "provider", p.ID, "pool", exchanger, "error", err)
		return domain.ExternalAccountID{}, ErrExchangeFailed
	}
	return c.linkOAuthAccount(ctx, userID, p, conn, tok, providerAcctID, requestedWorker)
}

// linkOAuthAccount creates the account for an exchanged token and stores it.
func (c *Connections) linkOAuthAccount(ctx context.Context, userID domain.UserID, p domain.ProviderMeta, conn domain.UserProviderConfig, tok port.TokenState, providerAcctID, requestedWorker string) (domain.ExternalAccountID, error) {
	label := p.DisplayName
	if conn.Label != "" {
		label = conn.Label
	}
	pool, err := c.pool(ctx, userID, p.ID, conn.ID, providerAcctID, requestedWorker)
	if err != nil {
		c.Log.Warn("oauth connect: no worker pool", "provider", p.ID, "error", err)
		return domain.ExternalAccountID{}, ErrChooseWorker
	}
	held, err := c.Accounts.HeldByOtherUser(ctx, p.ID, providerAcctID, userID)
	if err != nil {
		c.Log.Error("oauth connect: holder check failed", "provider", p.ID, "error", err)
		return domain.ExternalAccountID{}, port.NewActionError(port.ActionErrorInternal, "could not save account")
	}
	if held {
		c.Log.Warn("oauth connect: provider account already linked to another user",
			"provider", p.ID, "provider_account_id", providerAcctID, "connection_id", conn.ID)
		return domain.ExternalAccountID{}, ErrAlreadyLinked
	}
	scopes := []string{}
	if p.OAuth2 != nil {
		scopes = append(scopes, p.OAuth2.Scopes...)
	}
	acctID, err := c.Accounts.CreateAccount(ctx, domain.ExternalAccount{
		UserID: userID, Provider: p.ID, ProviderAccountID: providerAcctID,
		ConnectionID: &conn.ID, DisplayLabel: label, GrantedScopes: scopes,
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
	c.Log.Info("oauth account connected", "provider", p.ID, "account_id", acctID, "connection_id", conn.ID)
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
