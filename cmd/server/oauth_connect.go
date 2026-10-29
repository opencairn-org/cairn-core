package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// providerMeta is a provider's OAuth2 endpoint knowledge (URLs + scopes). The
// CREDENTIALS are per-CONNECTION: a user can create several connections for the
// same provider, EACH with its own client_id/client_secret.
type providerMeta struct {
	Provider     string
	DisplayName  string
	AuthorizeURL string
	TokenURL     string
	Scopes       string
	// ActivityURLTemplate builds a deep link to the original activity on the
	// provider's site; "{id}" is replaced with the provider external id. Empty
	// when the provider has no public activity URL.
	ActivityURLTemplate string
}

var providerRegistry = map[string]providerMeta{
	"strava": { //nolint:gosec // G101: public OAuth endpoint URLs, no credentials
		Provider:            "strava",
		DisplayName:         "Strava",
		AuthorizeURL:        "https://www.strava.com/oauth/authorize",
		TokenURL:            "https://www.strava.com/oauth/token",
		Scopes:              "read,activity:read_all",
		ActivityURLTemplate: "https://www.strava.com/activities/{id}",
	},
	// Credential-based provider: no OAuth endpoints. client_id/client_secret
	// hold the Garmin Connect email/password; the account is created together
	// with the connection (no authorize round-trip to link it).
	"garmin": {
		Provider:            "garmin",
		DisplayName:         "Garmin",
		ActivityURLTemplate: "https://connect.garmin.com/modern/activity/{id}",
	},
}

// oauthProvider reports whether a provider links accounts via an OAuth
// authorize round-trip (vs. credential-based providers like Garmin).
func oauthProvider(meta providerMeta) bool { return meta.AuthorizeURL != "" }

// providerActivityURL builds the external deep link for a provider's activity,
// or "" when the provider has no template.
func providerActivityURL(provider, externalID string) string {
	meta, ok := providerRegistry[provider]
	if !ok || meta.ActivityURLTemplate == "" || externalID == "" {
		return ""
	}
	return strings.ReplaceAll(meta.ActivityURLTemplate, "{id}", externalID)
}

const (
	oauthStateCookie      = "cairn_oauth_state"
	oauthConnectionCookie = "cairn_oauth_connection"
	oauthWorkerCookie     = "cairn_oauth_worker"
)

// mountOAuthConnect wires connection CRUD + the OAuth account-connect flow:
//
//	GET    /api/connections                       list connections + provider types
//	POST   /api/connections                       { provider, label, client_id, client_secret }
//	PUT    /api/connections/{id}                  { label, client_id, client_secret }
//	DELETE /api/connections/{id}
//	GET    /auth/oauth/{provider}/connect?connection=<id>   start OAuth for that connection
//	GET    /auth/oauth/{provider}/callback                  exchange → account (linked to connection)
func mountOAuthConnect(mux *http.ServeMux, app *App, logger *slog.Logger, publicURL string) {
	h := &oauthConnectHandlers{app: app, logger: logger, publicURL: publicURL}

	// GET /api/connections
	mux.HandleFunc("GET /api/connections", h.list)

	// GET /api/webhook-endpoints — the webhook callback URLs to register with a
	// provider's app for real-time imports. One per webhook-advertising worker
	// ({name}-{provider}); the worker owns verification/decoding. The callback
	// URL is not a secret (it's a public endpoint), so any signed-in user may
	// read it — they need it to configure their own provider app.
	mux.HandleFunc("GET /api/webhook-endpoints", h.webhookEndpoints)

	// POST /api/connections — create a connection.
	mux.HandleFunc("POST /api/connections", h.create)

	// PUT /api/connections/{id} — update a connection (ownership-checked).
	mux.HandleFunc("PUT /api/connections/{id}", h.update)

	// DELETE /api/connections/{id}
	mux.HandleFunc("DELETE /api/connections/{id}", h.delete)

	mux.HandleFunc("GET /auth/oauth/{provider}/connect", h.connect)
	mux.HandleFunc("GET /auth/oauth/{provider}/callback", h.callback)

	logger.Info("oauth connect endpoints mounted", "providers", len(providerRegistry))
}

// oauthConnectHandlers carries the shared state of the connection routes.
type oauthConnectHandlers struct {
	app       *App
	logger    *slog.Logger
	publicURL string
}

func (h *oauthConnectHandlers) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	conns, providers, err := h.app.Accounts.Connections(r.Context(), userID)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(conns))
	for _, c := range conns {
		out = append(out, map[string]any{
			"id":           c.Connection.ID.String(),
			"provider":     c.Connection.Provider,
			"display_name": c.DisplayName,
			"label":        c.Connection.Label,
			"client_id":    c.Connection.ClientID,
			"has_secret":   c.Connection.HasSecret,
			"oauth":        c.OAuth,
		})
	}
	// Known pools per provider: with several, the user picks which one a
	// connection is for (docs/adr/0006).
	provOut := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		pools := make([]map[string]any, 0, len(p.Pools))
		for _, wp := range p.Pools {
			pools = append(pools, map[string]any{
				"name": wp.Name, "package": wp.Package, "parser_version": wp.ParserVersion, "release": wp.Release,
				"online": wp.Online, "compatible": wp.Compatible,
			})
		}
		provOut = append(provOut, map[string]any{
			"provider": p.Provider.ID, "display_name": p.Provider.DisplayName, "oauth": p.Provider.OAuth,
			"worker_pools": pools,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": out, "providers": provOut})
}

// oauthConnectWebhookEP is one advertised webhook callback.
type oauthConnectWebhookEP struct {
	Provider  string `json:"provider"`
	WorkerKey string `json:"worker_key"`
	URL       string `json:"url"`
}

func (h *oauthConnectHandlers) webhookEndpoints(w http.ResponseWriter, r *http.Request) {
	if _, ok := resolveSessionUser(r, h.app); !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	out := []oauthConnectWebhookEP{}
	if h.app.NATSBus != nil {
		if kv, err := h.app.NATSBus.KV("cairn_worker_presence"); err == nil {
			out = h.collectWebhookEndpoints(r.Context(), kv, out)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": out})
}

func (h *oauthConnectHandlers) collectWebhookEndpoints(ctx context.Context, kv port.KV, out []oauthConnectWebhookEP) []oauthConnectWebhookEP {
	seen := map[string]bool{}
	keys, _ := kv.Keys(ctx)
	for _, k := range keys {
		entry, err := kv.Get(ctx, k)
		if err != nil {
			// Presence keys carry a TTL; one can expire between Keys and Get.
			h.logger.Debug("webhook endpoints: presence key vanished", "key", k, "error", err)
			continue
		}
		var hb struct {
			WorkerKey string `json:"worker_key"`
			Provider  string `json:"provider"`
			Webhooks  bool   `json:"webhooks"`
		}
		if json.Unmarshal(entry.Value, &hb) != nil || !hb.Webhooks || hb.WorkerKey == "" {
			continue
		}
		if seen[hb.WorkerKey] {
			continue
		}
		seen[hb.WorkerKey] = true
		out = append(out, oauthConnectWebhookEP{
			Provider:  hb.Provider,
			WorkerKey: hb.WorkerKey,
			URL:       strings.TrimRight(h.publicURL, "/") + "/webhooks/" + hb.WorkerKey,
		})
	}
	return out
}

// oauthConnectCreateBody is the POST /api/connections payload.
type oauthConnectCreateBody struct {
	Provider     string `json:"provider"`
	Label        string `json:"label"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	// Worker is the pool the account is connected for (docs/adr/0006);
	// required only when several pools serve the provider.
	Worker string `json:"worker"`
}

func (h *oauthConnectHandlers) create(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body oauthConnectCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	meta, ok := providerRegistry[body.Provider]
	if !ok {
		http.Error(w, "unknown provider", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.ClientID) == "" {
		http.Error(w, "client_id is required", http.StatusBadRequest)
		return
	}
	if !oauthProvider(meta) && strings.TrimSpace(body.ClientSecret) == "" {
		// Credential providers: client_id/client_secret are the login —
		// without the secret the connection could never authenticate.
		http.Error(w, "client_secret is required", http.StatusBadRequest)
		return
	}
	id, err := h.app.UserProviderConfigs.Create(r.Context(), domain.UserProviderConfig{
		UserID: userID, Provider: body.Provider, Label: strings.TrimSpace(body.Label),
		ClientID: strings.TrimSpace(body.ClientID), ClientSecret: strings.TrimSpace(body.ClientSecret),
	})
	if err != nil {
		h.logger.Error("create connection failed", "error", err)
		http.Error(w, "could not create", http.StatusInternalServerError)
		return
	}
	// Credential providers have no authorize round-trip: the credentials ARE
	// the account link, so create the external account with the connection.
	if !oauthProvider(meta) && h.app.ExternalAccounts != nil {
		if err := h.linkCredentialAccount(r.Context(), userID, id, body); err != nil {
			switch {
			case errors.Is(err, domain.ErrWorkerAmbiguous), errors.Is(err, domain.ErrNoWorker):
				http.Error(w, err.Error(), http.StatusBadRequest)
			default:
				http.Error(w, "could not create", http.StatusInternalServerError)
			}
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id.String()})
}

// linkCredentialAccount creates the external account for a credential provider;
// on failure it rolls the connection back.
func (h *oauthConnectHandlers) linkCredentialAccount(ctx context.Context, userID domain.UserID, id domain.ConnectionID, body oauthConnectCreateBody) error {
	login := strings.TrimSpace(body.ClientID)
	pool, err := connectPool(ctx, h.app, h.logger, userID, body.Provider, id, login, body.Worker)
	var acctID domain.ExternalAccountID
	if err == nil {
		acctID, err = h.app.ExternalAccounts.CreateAccount(ctx, domain.ExternalAccount{
			UserID:             userID,
			Provider:           body.Provider,
			ProviderAccountID:  login,
			ConnectionID:       &id,
			DisplayLabel:       login,
			AssignedWorkerName: pool,
			Status:             domain.ExternalAccountStatusActive,
			AutoImportEnabled:  true,
		})
	}
	if err != nil {
		// Roll the config back so the user can retry cleanly instead of
		// being left with a credential-less half-connection.
		_ = h.app.UserProviderConfigs.Delete(ctx, id)
		h.logger.Error("create connection: account link failed", "provider", body.Provider, "error", err)
		return err
	}
	h.logger.Info("connection + account created", "provider", body.Provider, "account", acctID, "worker", pool)
	syncGearOnConnect(ctx, h.app, h.logger, acctID)
	return nil
}

// connectPool resolves the worker pool a connection's account is created for
// (domain.ResolveConnectPool): the user's choice, else the pool this
// connection already used for the same provider account, else the only one.
func connectPool(ctx context.Context, app *App, logger *slog.Logger, userID domain.UserID, provider string, conn domain.ConnectionID, providerAcctID, requested string) (string, error) {
	var existing string
	if accts, err := app.ExternalAccounts.ListAccountsForUser(ctx, userID); err == nil {
		for _, a := range accts {
			if a.Provider == provider && a.ProviderAccountID == providerAcctID && a.ConnectionID != nil && *a.ConnectionID == conn {
				existing = a.AssignedWorkerName
			}
		}
	}
	return domain.ResolveConnectPool(requested, existing, knownPoolNames(ctx, app, logger, provider))
}

func (h *oauthConnectHandlers) update(w http.ResponseWriter, r *http.Request) {
	conn, ok := ownConnection(r, h.app)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Label        string `json:"label"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	conn.Label = strings.TrimSpace(body.Label)
	if strings.TrimSpace(body.ClientID) != "" {
		conn.ClientID = strings.TrimSpace(body.ClientID)
	}
	conn.ClientSecret = strings.TrimSpace(body.ClientSecret) // empty keeps existing
	if err := h.app.UserProviderConfigs.Update(r.Context(), conn); err != nil {
		http.Error(w, "could not update", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *oauthConnectHandlers) delete(w http.ResponseWriter, r *http.Request) {
	conn, ok := ownConnection(r, h.app)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := h.app.UserProviderConfigs.Delete(r.Context(), conn.ID); err != nil {
		http.Error(w, "could not delete", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// oauthProviderFromPath resolves {provider} to an OAuth provider, writing the
// error response itself on failure.
func (h *oauthConnectHandlers) oauthProviderFromPath(w http.ResponseWriter, r *http.Request) (domain.UserID, providerMeta, bool) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return userID, providerMeta{}, false
	}
	meta, ok := providerRegistry[r.PathValue("provider")]
	if !ok {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return userID, providerMeta{}, false
	}
	if !oauthProvider(meta) {
		http.Error(w, "provider does not use OAuth", http.StatusBadRequest)
		return userID, providerMeta{}, false
	}
	return userID, meta, true
}

func (h *oauthConnectHandlers) connect(w http.ResponseWriter, r *http.Request) {
	userID, meta, ok := h.oauthProviderFromPath(w, r)
	if !ok {
		return
	}
	connID, err := uuid.Parse(r.URL.Query().Get("connection"))
	if err != nil {
		http.Redirect(w, r, "/connections?error=no_connection", http.StatusFound)
		return
	}
	conn, err := h.app.UserProviderConfigs.GetByID(r.Context(), domain.ConnectionID(connID))
	if err != nil || conn.UserID != userID || conn.Provider != meta.Provider || conn.ClientID == "" {
		http.Redirect(w, r, "/connections?error=configure_first", http.StatusFound)
		return
	}
	// Secret stored but unreadable (AAD/key change) — the token exchange in
	// the callback would fail. Tell the user to re-enter it rather than
	// bouncing them through Strava only to fail at the end.
	if conn.SecretUnreadable {
		http.Redirect(w, r, "/connections?error=secret_unreadable", http.StatusFound)
		return
	}

	// The pool to connect for travels through the round-trip (docs/adr/0006).
	worker := r.URL.Query().Get("worker")
	if worker != "" && !domain.ValidWorkerName(worker) {
		http.Redirect(w, r, "/connections?error=bad_worker", http.StatusFound)
		return
	}

	state := randToken()
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name: oauthStateCookie, Value: state, Path: "/auth/oauth/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: cookieSecure(r), MaxAge: 600,
	})
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name: oauthWorkerCookie, Value: worker, Path: "/auth/oauth/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: cookieSecure(r), MaxAge: 600,
	})
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name: oauthConnectionCookie, Value: conn.ID.String(), Path: "/auth/oauth/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: cookieSecure(r), MaxAge: 600,
	})
	q := url.Values{}
	q.Set("client_id", conn.ClientID)
	q.Set("redirect_uri", strings.TrimRight(h.publicURL, "/")+"/auth/oauth/"+meta.Provider+"/callback")
	q.Set("response_type", "code")
	q.Set("scope", meta.Scopes)
	// force so the provider always shows its auth screen — lets a user pick a
	// different account for a separate connection.
	q.Set("approval_prompt", "force")
	q.Set("state", state)
	http.Redirect(w, r, meta.AuthorizeURL+"?"+q.Encode(), http.StatusFound)
}

// callbackConnection checks state, reads the connection cookie and the code,
// and loads the user's connection; it writes the response itself on failure.
func (h *oauthConnectHandlers) callbackConnection(w http.ResponseWriter, r *http.Request, userID domain.UserID) (domain.UserProviderConfig, string, bool) {
	c, err := r.Cookie(oauthStateCookie)
	if err != nil || c.Value == "" || c.Value != r.URL.Query().Get("state") {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return domain.UserProviderConfig{}, "", false
	}
	cc, err := r.Cookie(oauthConnectionCookie)
	if err != nil || cc.Value == "" {
		http.Redirect(w, r, "/connections?error=no_connection", http.StatusFound)
		return domain.UserProviderConfig{}, "", false
	}
	connUUID, err := uuid.Parse(cc.Value)
	if err != nil {
		http.Redirect(w, r, "/connections?error=no_connection", http.StatusFound)
		return domain.UserProviderConfig{}, "", false
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Redirect(w, r, "/connections?error="+url.QueryEscape(e), http.StatusFound)
		return domain.UserProviderConfig{}, "", false
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return domain.UserProviderConfig{}, "", false
	}
	conn, err := h.app.UserProviderConfigs.GetByID(r.Context(), domain.ConnectionID(connUUID))
	if err != nil || conn.UserID != userID {
		http.Redirect(w, r, "/connections?error=configure_first", http.StatusFound)
		return domain.UserProviderConfig{}, "", false
	}
	return conn, code, true
}

func (h *oauthConnectHandlers) callback(w http.ResponseWriter, r *http.Request) {
	userID, meta, ok := h.oauthProviderFromPath(w, r)
	if !ok {
		return
	}
	conn, code, ok := h.callbackConnection(w, r, userID)
	if !ok {
		return
	}

	tok, providerAcctID, err := exchangeOAuthCode(r.Context(), meta, conn, code)
	if err != nil {
		h.logger.Error("oauth code exchange failed", "provider", meta.Provider, "error", err)
		http.Redirect(w, r, "/connections?error=exchange_failed", http.StatusFound)
		return
	}

	connID := conn.ID
	label := meta.DisplayName
	if conn.Label != "" {
		label = conn.Label
	}
	var requested string
	if c, err := r.Cookie(oauthWorkerCookie); err == nil {
		requested = c.Value
	}
	pool, err := connectPool(r.Context(), h.app, h.logger, userID, meta.Provider, connID, providerAcctID, requested)
	if err != nil {
		h.logger.Warn("oauth connect: no worker pool", "provider", meta.Provider, "error", err)
		http.Redirect(w, r, "/connections?error=choose_worker", http.StatusFound)
		return
	}
	acctID, err := h.app.ExternalAccounts.CreateAccount(r.Context(), domain.ExternalAccount{
		UserID: userID, Provider: meta.Provider, ProviderAccountID: providerAcctID,
		ConnectionID: &connID, DisplayLabel: label, GrantedScopes: splitScopes(meta.Scopes),
		AssignedWorkerName: pool,
	})
	if err != nil {
		h.logger.Error("create external account failed", "error", err)
		http.Error(w, "could not save account", http.StatusInternalServerError)
		return
	}
	if err := h.app.OAuthTokens.StoreToken(r.Context(), acctID, port.StoreTokenInput{State: tok}); err != nil {
		h.logger.Error("store oauth token failed", "error", err)
		http.Error(w, "could not save token", http.StatusInternalServerError)
		return
	}
	h.logger.Info("oauth account connected", "provider", meta.Provider, "account_id", acctID, "connection_id", connID)
	syncGearOnConnect(r.Context(), h.app, h.logger, acctID)
	http.Redirect(w, r, "/connections?connected="+meta.Provider, http.StatusFound)
}

// ownConnection resolves {id} and confirms it belongs to the session user.
func ownConnection(r *http.Request, app *App) (domain.UserProviderConfig, bool) {
	userID, ok := resolveSessionUser(r, app)
	if !ok {
		return domain.UserProviderConfig{}, false
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return domain.UserProviderConfig{}, false
	}
	conn, err := app.UserProviderConfigs.GetByID(r.Context(), domain.ConnectionID(id))
	if err != nil || conn.UserID != userID {
		return domain.UserProviderConfig{}, false
	}
	return conn, true
}

// exchangeOAuthCode posts the authorization code to the provider's token
// endpoint using the connection's app credentials.
func exchangeOAuthCode(ctx context.Context, meta providerMeta, conn domain.UserProviderConfig, code string) (port.TokenState, string, error) {
	form := url.Values{}
	form.Set("client_id", conn.ClientID)
	form.Set("client_secret", conn.ClientSecret)
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return port.TokenState{}, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return port.TokenState{}, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return port.TokenState{}, "", fmt.Errorf("token endpoint %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    int64  `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		Athlete      struct {
			ID int64 `json:"id"`
		} `json:"athlete"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return port.TokenState{}, "", err
	}

	exp := time.Unix(body.ExpiresAt, 0)
	if body.ExpiresAt == 0 && body.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	ts := port.TokenState{
		AccessToken: body.AccessToken, RefreshToken: body.RefreshToken,
		ExpiresAt: exp, Scope: body.Scope, TokenType: body.TokenType,
	}
	acctID := ""
	if body.Athlete.ID != 0 {
		acctID = strconv.FormatInt(body.Athlete.ID, 10)
	}
	return ts, acctID, nil
}

func splitScopes(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func randToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
