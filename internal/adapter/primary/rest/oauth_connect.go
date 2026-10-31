package rest

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/providerlink"
)

// Cookies carrying the connect round-trip's state across the provider.
const (
	OAuthConnectStateCookie = "cairn_oauth_state"
	oauthConnectionCookie   = "cairn_oauth_connection"
	oauthWorkerCookie       = "cairn_oauth_worker"
)

// ConnectionHandlers serve a user's provider connections and the OAuth
// account-connect round-trip.
//
//	GET    /api/connections                      list connections + provider types
//	GET    /api/webhook-endpoints                webhook callback URLs per worker
//	POST   /api/connections                      { provider, label, client_id, client_secret, worker }
//	PUT    /api/connections/{id}                 { label, client_id, client_secret }
//	DELETE /api/connections/{id}
//	GET    /auth/oauth/{provider}/connect?connection=<id>  start OAuth for that connection
//	GET    /auth/oauth/{provider}/callback                 exchange → account
type ConnectionHandlers struct {
	Auth        *Auth
	Connections *providerlink.Connections
	Accounts    *providerlink.Accounts
	Log         *slog.Logger
	// PublicURL is the instance's external base URL.
	PublicURL string
	// SecureCookie reports whether cookies set on r must be Secure.
	SecureCookie func(r *http.Request) bool
}

// Mount registers the routes.
func (h *ConnectionHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/connections", h.list)
	// The callback URL is a public endpoint, not a secret: any signed-in user
	// needs it to configure their own provider app.
	mux.HandleFunc("GET /api/webhook-endpoints", h.webhookEndpoints)
	mux.HandleFunc("POST /api/connections", h.create)
	mux.HandleFunc("PUT /api/connections/{id}", h.update)
	mux.HandleFunc("DELETE /api/connections/{id}", h.delete)
	mux.HandleFunc("GET /auth/oauth/{provider}/connect", h.connect)
	mux.HandleFunc("GET /auth/oauth/{provider}/callback", h.callback)
}

func (h *ConnectionHandlers) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	conns, providers, err := h.Accounts.Connections(r.Context(), userID)
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

type oauthConnectWebhookEP struct {
	Provider  string `json:"provider"`
	WorkerKey string `json:"worker_key"`
	URL       string `json:"url"`
}

func (h *ConnectionHandlers) webhookEndpoints(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.Auth.requireUser(w, r); !ok {
		return
	}
	eps := h.Connections.WebhookEndpoints(r.Context(), h.PublicURL)
	out := make([]oauthConnectWebhookEP, 0, len(eps))
	for _, e := range eps {
		out = append(out, oauthConnectWebhookEP(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": out})
}

func (h *ConnectionHandlers) create(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok || !requireJSON(w, r) {
		return
	}
	var body struct {
		Provider     string `json:"provider"`
		Label        string `json:"label"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Worker       string `json:"worker"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	id, err := h.Connections.Create(r.Context(), userID, providerlink.NewConnection(body))
	if err != nil {
		writeActionError(w, err, "could not create")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id.String()})
}

// own resolves {id} to the caller's connection; anything else is a 404,
// anonymous callers included.
func (h *ConnectionHandlers) own(w http.ResponseWriter, r *http.Request) (domain.UserProviderConfig, bool) {
	userID, ok := h.Auth.User(r)
	var conn domain.UserProviderConfig
	if ok {
		id, err := domain.ParseUUID[domain.ConnectionID](r.PathValue("id"))
		if err == nil {
			conn, err = h.Connections.Own(r.Context(), userID, id)
		}
		ok = err == nil
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
	}
	return conn, ok
}

func (h *ConnectionHandlers) update(w http.ResponseWriter, r *http.Request) {
	conn, ok := h.own(w, r)
	if !ok || !requireJSON(w, r) {
		return
	}
	var body struct {
		Label        string `json:"label"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.Connections.Update(r.Context(), conn, body.Label, body.ClientID, body.ClientSecret); err != nil {
		http.Error(w, "could not update", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *ConnectionHandlers) delete(w http.ResponseWriter, r *http.Request) {
	conn, ok := h.own(w, r)
	if !ok {
		return
	}
	if err := h.Connections.Delete(r.Context(), conn); err != nil {
		http.Error(w, "could not delete", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// oauthProvider resolves the caller and {provider} to an OAuth provider,
// writing the error response itself on failure.
func (h *ConnectionHandlers) oauthProvider(w http.ResponseWriter, r *http.Request) (domain.UserID, providerlink.Provider, bool) {
	userID, ok := h.Auth.User(r)
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return userID, providerlink.Provider{}, false
	}
	p, err := h.Connections.OAuthProvider(r.PathValue("provider"))
	switch {
	case errors.Is(err, providerlink.ErrUnknownProvider):
		http.Error(w, err.Error(), http.StatusNotFound)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
	return userID, p, err == nil
}

// oauthConnectFailed sends the user back to the connections page with a reason.
func oauthConnectFailed(w http.ResponseWriter, r *http.Request, reason string) {
	http.Redirect(w, r, "/connections?error="+url.QueryEscape(reason), http.StatusFound)
}

func (h *ConnectionHandlers) callbackURL(p providerlink.Provider) string {
	return strings.TrimRight(h.PublicURL, "/") + "/auth/oauth/" + p.Provider + "/callback"
}

func (h *ConnectionHandlers) setRoundTripCookie(w http.ResponseWriter, r *http.Request, name, value string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name: name, Value: value, Path: "/auth/oauth/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.SecureCookie != nil && h.SecureCookie(r), MaxAge: 600,
	})
}

func (h *ConnectionHandlers) connect(w http.ResponseWriter, r *http.Request) {
	userID, p, ok := h.oauthProvider(w, r)
	if !ok {
		return
	}
	connID, err := domain.ParseUUID[domain.ConnectionID](r.URL.Query().Get("connection"))
	if err != nil {
		oauthConnectFailed(w, r, string(providerlink.ErrNoConnection))
		return
	}
	// The pool to connect for travels through the round-trip (docs/adr/0006).
	worker := r.URL.Query().Get("worker")
	state := oauthRandToken()
	target, err := h.Connections.AuthorizeURL(r.Context(), userID, p, connID, worker, state, h.callbackURL(p))
	if err != nil {
		var ce providerlink.ConnectError
		if errors.As(err, &ce) {
			oauthConnectFailed(w, r, string(ce))
			return
		}
		writeActionError(w, err, "connect failed")
		return
	}
	h.setRoundTripCookie(w, r, OAuthConnectStateCookie, state)
	h.setRoundTripCookie(w, r, oauthWorkerCookie, worker)
	h.setRoundTripCookie(w, r, oauthConnectionCookie, connID.String())
	http.Redirect(w, r, target, http.StatusFound) //nolint:gosec // G710: the provider registry's fixed authorize endpoint
}

// callbackParams checks the state and reads the connection cookie and the
// code; it writes the response itself on failure.
func (h *ConnectionHandlers) callbackParams(w http.ResponseWriter, r *http.Request) (domain.ConnectionID, string, bool) {
	q := r.URL.Query()
	if c, err := r.Cookie(OAuthConnectStateCookie); err != nil || c.Value == "" || c.Value != q.Get("state") {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return domain.ConnectionID{}, "", false
	}
	cc, err := r.Cookie(oauthConnectionCookie)
	if err != nil || cc.Value == "" {
		oauthConnectFailed(w, r, string(providerlink.ErrNoConnection))
		return domain.ConnectionID{}, "", false
	}
	connID, err := domain.ParseUUID[domain.ConnectionID](cc.Value)
	if err != nil {
		oauthConnectFailed(w, r, string(providerlink.ErrNoConnection))
		return domain.ConnectionID{}, "", false
	}
	if e := q.Get("error"); e != "" {
		oauthConnectFailed(w, r, e)
		return domain.ConnectionID{}, "", false
	}
	code := q.Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return domain.ConnectionID{}, "", false
	}
	return connID, code, true
}

func (h *ConnectionHandlers) callback(w http.ResponseWriter, r *http.Request) {
	userID, p, ok := h.oauthProvider(w, r)
	if !ok {
		return
	}
	connID, code, ok := h.callbackParams(w, r)
	if !ok {
		return
	}
	var requested string
	if c, err := r.Cookie(oauthWorkerCookie); err == nil {
		requested = c.Value
	}
	if _, err := h.Connections.Complete(r.Context(), userID, p, connID, code, requested); err != nil {
		var ce providerlink.ConnectError
		if errors.As(err, &ce) {
			oauthConnectFailed(w, r, string(ce))
			return
		}
		writeActionError(w, err, "could not save account")
		return
	}
	http.Redirect(w, r, "/connections?connected="+p.Provider, http.StatusFound)
}

func oauthRandToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// AccountJobHandlers trigger worker jobs on one of the caller's provider
// accounts.
//
//	POST /api/accounts/{id}/sync-gear       queue a gear import
//	POST /api/accounts/{id}/import-health   queue a health-metric import (?days=1..365)
type AccountJobHandlers struct {
	Auth *Auth
	Jobs *providerlink.Jobs
	// HealthImport enables the import-health route.
	HealthImport bool
}

// Mount registers the routes.
func (h *AccountJobHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/accounts/{id}/sync-gear", h.syncGear)
	if h.HealthImport {
		mux.HandleFunc("POST /api/accounts/{id}/import-health", h.importHealth)
	}
}

func oauthAccountID(w http.ResponseWriter, r *http.Request) (domain.ExternalAccountID, bool) {
	id, err := domain.ParseUUID[domain.ExternalAccountID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
	}
	return id, err == nil
}

func (h *AccountJobHandlers) syncGear(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	accID, ok := oauthAccountID(w, r)
	if !ok {
		return
	}
	if err := h.Jobs.SyncGear(r.Context(), userID, accID); err != nil {
		writeActionError(w, err, "enqueue failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": true})
}

func (h *AccountJobHandlers) importHealth(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !h.Jobs.Available() {
		http.Error(w, "worker bus unavailable", http.StatusServiceUnavailable)
		return
	}
	accID, ok := oauthAccountID(w, r)
	if !ok {
		return
	}
	days := providerlink.HealthImportDays(parseIntQuery(r, "days", 30))
	if err := h.Jobs.ImportHealth(r.Context(), userID, accID, days); err != nil {
		writeActionError(w, err, "enqueue failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": true, "days": days})
}
