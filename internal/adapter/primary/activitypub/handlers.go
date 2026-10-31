// Package activitypub is the ActivityPub primary adapter: WebFinger,
// NodeInfo, the Person actor with its collections, and the HTTP-signature
// verified inbox. Handlers get use cases only.
package activitypub

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/httpsig"
	"github.com/opencairn-org/cairn-core/internal/usecase/federation"
)

const contentType = "application/activity+json"

// Handlers serve the federation protocol routes.
//
//	GET  /.well-known/webfinger        acct:user@host → actor id
//	GET  /.well-known/nodeinfo         NodeInfo discovery
//	GET  /nodeinfo/2.1                 instance metadata
//	GET  /users/{username}             Person actor
//	GET  /users/{username}/followers   follower count
//	GET  /users/{username}/following   following count
//	GET  /users/{username}/outbox      public activities, paged
//	POST /users/{username}/inbox       signed inbound activities
type Handlers struct {
	Actors *federation.Actors
	Inbox  *federation.Inbox
	Remote *federation.Remote
	// InboxAllow is the per-remote-domain inbox rate limit (DoS guard).
	InboxAllow func(host string) bool
	// Version is the software version NodeInfo reports.
	Version string
	Log     *slog.Logger
}

// Mount registers the routes.
func (h *Handlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/webfinger", h.webfinger)
	mux.HandleFunc("GET /.well-known/nodeinfo", h.nodeinfoLinks)
	mux.HandleFunc("GET /nodeinfo/2.1", h.nodeinfo)
	mux.HandleFunc("GET /users/{username}", h.actor)
	mux.HandleFunc("GET /users/{username}/followers", h.followers)
	mux.HandleFunc("GET /users/{username}/following", h.following)
	mux.HandleFunc("GET /users/{username}/outbox", h.outbox)
	mux.HandleFunc("POST /users/{username}/inbox", h.inbox)
}

func (h *Handlers) instance() federation.Instance { return h.Actors.Instance }

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

// writeAP writes an ActivityPub document.
func writeAP(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", contentType)
	_ = json.NewEncoder(w).Encode(body)
}

// parseAcct extracts the username of "acct:username@host" when host is this instance.
func parseAcct(resource, host string) (string, bool) {
	resource = strings.TrimPrefix(strings.TrimSpace(resource), "acct:")
	at := strings.LastIndexByte(resource, '@')
	if at <= 0 {
		return "", false
	}
	user, dom := resource[:at], resource[at+1:]
	if user == "" || !strings.EqualFold(dom, host) {
		return "", false
	}
	return user, true
}

func (h *Handlers) webfinger(w http.ResponseWriter, r *http.Request) {
	in := h.instance()
	username, ok := parseAcct(r.URL.Query().Get("resource"), in.Host)
	if !ok {
		http.Error(w, "bad resource", http.StatusBadRequest)
		return
	}
	u, ok := h.Actors.Resolve(r.Context(), username)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, map[string]any{
		"subject": "acct:" + u.Username + "@" + in.Host,
		"links": []any{
			map[string]any{"rel": "self", "type": contentType, "href": in.ActorID(u.Username)},
		},
	})
}

func (h *Handlers) nodeinfoLinks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"links": []any{map[string]any{
			"rel":  "http://nodeinfo.diaspora.software/ns/schema/2.1",
			"href": h.instance().Base + "/nodeinfo/2.1",
		}},
	})
}

func (h *Handlers) nodeinfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"version":           "2.1",
		"software":          map[string]any{"name": "cairn", "version": h.Version},
		"protocols":         []string{"activitypub"},
		"services":          map[string]any{"inbound": []string{}, "outbound": []string{}},
		"openRegistrations": false,
		// User/post counts omitted on purpose (privacy on a personal instance).
		"usage":    map[string]any{"users": map[string]any{}},
		"metadata": map[string]any{"nodeName": "Cairn"},
	})
}

func (h *Handlers) actor(w http.ResponseWriter, r *http.Request) {
	u, ok := h.Actors.Resolve(r.Context(), r.PathValue("username"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	pubPEM, err := h.Actors.PublicKeyPEM(r.Context(), u.ID)
	if err != nil {
		h.Log.Error("federation: key load failed", "user", u.ID, "error", err)
		http.Error(w, "key error", http.StatusInternalServerError)
		return
	}
	in := h.instance()
	id := in.ActorID(u.Username)
	name := u.DisplayName
	if name == "" {
		name = u.Username
	}
	writeAP(w, map[string]any{
		"@context":          []any{"https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1"},
		"type":              "Person",
		"id":                id,
		"preferredUsername": u.Username,
		"name":              name,
		"url":               in.Base + "/u/" + u.Username,
		"inbox":             id + "/inbox",
		"outbox":            id + "/outbox",
		"followers":         id + "/followers",
		"following":         id + "/following",
		"publicKey": map[string]any{
			"id":           id + "#main-key",
			"owner":        id,
			"publicKeyPem": pubPEM,
		},
	})
}

// orderedCollection carries a totalItems count without inlining items.
func orderedCollection(id string, total int) map[string]any {
	return map[string]any{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"type":         "OrderedCollection",
		"id":           id,
		"totalItems":   total,
		"orderedItems": []any{},
	}
}

func (h *Handlers) followers(w http.ResponseWriter, r *http.Request) {
	h.collection(w, r, "/followers", func(c domain.FollowCounts) int { return c.Followers })
}

func (h *Handlers) following(w http.ResponseWriter, r *http.Request) {
	h.collection(w, r, "/following", func(c domain.FollowCounts) int { return c.Following })
}

func (h *Handlers) collection(w http.ResponseWriter, r *http.Request, suffix string, pick func(domain.FollowCounts) int) {
	u, ok := h.Actors.Resolve(r.Context(), r.PathValue("username"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeAP(w, orderedCollection(h.instance().ActorID(u.Username)+suffix, pick(h.Actors.Counts(r.Context(), u.ID))))
}

// outbox: the root carries metadata + a `first` link; ?page=true[&offset=N]
// returns an OrderedCollectionPage.
func (h *Handlers) outbox(w http.ResponseWriter, r *http.Request) {
	u, ok := h.Actors.Resolve(r.Context(), r.PathValue("username"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	outboxID := h.instance().ActorID(u.Username) + "/outbox"
	if r.URL.Query().Get("page") != "true" {
		writeAP(w, map[string]any{
			"@context":   "https://www.w3.org/ns/activitystreams",
			"type":       "OrderedCollection",
			"id":         outboxID,
			"totalItems": h.Actors.OutboxTotal(r.Context(), u.ID),
			"first":      outboxID + "?page=true",
		})
		return
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	p := h.Actors.Outbox(r.Context(), u, offset)
	page := map[string]any{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"type":         "OrderedCollectionPage",
		"id":           outboxID + "?page=true&offset=" + strconv.Itoa(offset),
		"partOf":       outboxID,
		"orderedItems": p.Items,
	}
	if offset+p.Fetched < p.Total {
		page["next"] = outboxID + "?page=true&offset=" + strconv.Itoa(offset+federation.OutboxPageSize)
	}
	writeAP(w, page)
}

func (h *Handlers) inbox(w http.ResponseWriter, r *http.Request) {
	target, ok := h.Actors.Resolve(r.Context(), r.PathValue("username"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	remoteActorID, remoteInbox, ok := h.authenticate(w, r, body)
	if !ok {
		return
	}
	err = h.Inbox.Receive(r.Context(), federation.Delivery{
		Target: target, Body: body, RemoteActorID: remoteActorID, RemoteInbox: remoteInbox,
	})
	if errors.Is(err, federation.ErrBadActivity) || errors.Is(err, federation.ErrWrongTarget) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// authenticate fetches the signer (by keyId) and verifies the signature,
// writing the error response itself on failure.
func (h *Handlers) authenticate(w http.ResponseWriter, r *http.Request, body []byte) (remoteActorID, remoteInbox string, ok bool) {
	keyID, ok := httpsig.KeyIDFromRequest(r)
	if !ok {
		http.Error(w, "unsigned request", http.StatusUnauthorized)
		return "", "", false
	}
	remoteActorID = strings.SplitN(keyID, "#", 2)[0]
	remoteHost := federation.HostOf(remoteActorID)
	// No host means no block or rate limit could apply: reject before the expensive fetch.
	if remoteHost == "" {
		http.Error(w, "bad actor", http.StatusBadRequest)
		return "", "", false
	}
	if h.Remote.Blocked(r.Context(), remoteHost) {
		http.Error(w, "blocked", http.StatusForbidden)
		return "", "", false
	}
	if h.InboxAllow != nil && !h.InboxAllow(remoteHost) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return "", "", false
	}
	remote, pub, err := h.Remote.Actor(r.Context(), remoteActorID)
	if err != nil {
		h.Log.Warn("federation: cannot fetch signer", "actor", remoteActorID, "error", err)
		http.Error(w, "cannot verify signer", http.StatusUnauthorized)
		return "", "", false
	}
	if err := httpsig.Verify(r, body, pub); err != nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return "", "", false
	}
	return remoteActorID, remote.Inbox, true
}
