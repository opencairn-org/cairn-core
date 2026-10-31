package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/httpsig"
)

// federation.go is the ActivityPub surface (docs/federation-design.md):
//   - Phase 1: discovery + actor — WebFinger, NodeInfo, the Person actor
//     document, and its followers/following/outbox collections.
//   - Phase 2: the inbox — HTTP-signature-verified Follow / Accept / Create,
//     plus outbound Follow (POST /api/federation/follow).
//   - Phase 3: outbound publishing — fresh public activities are pushed to
//     followers' inboxes (see federation_publish.go), hooked off the ingest
//     pipeline's follow-ups.
//
// Gated twice: the instance flag (CAIRN_FEDERATION_ENABLED, checked by the
// caller before mounting) AND the per-user opt-in (users.federation_enabled).
// A non-opted-in user 404s on every federation route — indistinguishable from
// "no such user".

const apContentType = "application/activity+json"

// fedHandlers carries the shared state of the federation routes.
type fedHandlers struct {
	app        *App
	logger     *slog.Logger
	base       string
	host       string
	inboxLimit *ipRateLimiter
}

func (h *fedHandlers) actorID(username string) string { return h.base + "/users/" + username }

// resolveActor returns the user iff federation is enabled for them.
func (h *fedHandlers) resolveActor(ctx context.Context, rawUsername string) (domain.User, bool) {
	u, err := h.app.Users.GetUserByUsername(ctx, strings.ToLower(strings.TrimSpace(rawUsername)))
	if err != nil {
		return domain.User{}, false
	}
	enabled, err := h.app.Users.IsFederationEnabled(ctx, u.ID)
	if err != nil || !enabled {
		return domain.User{}, false
	}
	return u, true
}

func mountFederation(mux *http.ServeMux, app *App, cfg *config.Config, logger *slog.Logger) {
	base := strings.TrimRight(cfg.HTTP.PublicBaseURL, "/")
	host := ""
	if u, err := url.Parse(base); err == nil {
		host = u.Host
	}

	// On a trusted private network (LAN self-hosting / the interop harness) the
	// SSRF dial guard would block peers on private IPs — drop it when the
	// operator explicitly opts in.
	if cfg.Federation.AllowPrivateHosts {
		federationClient = &http.Client{Timeout: 15 * time.Second}
		federationScheme = "http" // LAN/test peers serve plain HTTP
		logger.Warn("federation: SSRF dial guard disabled + WebFinger over http (CAIRN_FEDERATION_ALLOW_PRIVATE_HOSTS) — use only on trusted networks")
	}

	h := &fedHandlers{
		app: app, logger: logger, base: base, host: host,
		// Per-domain inbox rate limiter (DoS guard): ~120 inbound activities/min
		// per remote instance. Reuses the shared in-memory token bucket (which caps
		// its bucket map so forged-domain floods can't grow memory unbounded).
		inboxLimit: newIPRateLimiter(120, nil),
	}

	// --- WebFinger: acct:user@host -> actor id -------------------------------
	mux.HandleFunc("GET /.well-known/webfinger", h.webfinger)

	// --- NodeInfo: instance metadata discovery -------------------------------
	mux.HandleFunc("GET /.well-known/nodeinfo", h.nodeinfoLinks)
	mux.HandleFunc("GET /nodeinfo/2.1", fedNodeinfoDoc)

	// --- Actor document ------------------------------------------------------
	mux.HandleFunc("GET /users/{username}", h.actor)

	// --- Collections ---------------------------------------------------------
	mux.HandleFunc("GET /users/{username}/followers", h.followers)
	mux.HandleFunc("GET /users/{username}/following", h.following)
	mux.HandleFunc("GET /users/{username}/outbox", h.outbox)

	// --- Inbox: HTTP-signature-verified inbound activities -------------------
	mux.HandleFunc("POST /users/{username}/inbox", h.inbox)

	// --- Per-user opt-in toggle (session-gated) ------------------------------
	mux.HandleFunc("PUT /api/profile/federation", h.setOptIn)

	// --- Follow a remote actor (session-gated) -------------------------------
	mux.HandleFunc("POST /api/federation/follow", h.followRemote)

	// --- Admin: instance defederation blocklist (session + admin gated) ------
	mux.HandleFunc("GET /api/admin/federation/blocks", h.listBlocks)
	mux.HandleFunc("POST /api/admin/federation/blocks", h.addBlock)
	mux.HandleFunc("DELETE /api/admin/federation/blocks/{domain}", h.removeBlock)

	logger.Info("federation (ActivityPub phases 1–3) endpoints mounted",
		"host", host, "paths", []string{"/.well-known/webfinger", "/.well-known/nodeinfo", "/users/{username}"})
}

func (h *fedHandlers) webfinger(w http.ResponseWriter, r *http.Request) {
	username, ok := parseAcct(r.URL.Query().Get("resource"), h.host)
	if !ok {
		http.Error(w, "bad resource", http.StatusBadRequest)
		return
	}
	u, ok := h.resolveActor(r.Context(), username)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"subject": "acct:" + u.Username + "@" + h.host,
		"links": []any{
			map[string]any{"rel": "self", "type": apContentType, "href": h.actorID(u.Username)},
		},
	})
}

func (h *fedHandlers) nodeinfoLinks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"links": []any{map[string]any{
			"rel":  "http://nodeinfo.diaspora.software/ns/schema/2.1",
			"href": h.base + "/nodeinfo/2.1",
		}},
	})
}

func fedNodeinfoDoc(w http.ResponseWriter, _ *http.Request) {
	version, _, _ := buildInfo()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":           "2.1",
		"software":          map[string]any{"name": "cairn", "version": version},
		"protocols":         []string{"activitypub"},
		"services":          map[string]any{"inbound": []string{}, "outbound": []string{}},
		"openRegistrations": false,
		// User/post counts omitted on purpose (privacy on a personal instance).
		"usage":    map[string]any{"users": map[string]any{}},
		"metadata": map[string]any{"nodeName": "Cairn"},
	})
}

func (h *fedHandlers) actor(w http.ResponseWriter, r *http.Request) {
	u, ok := h.resolveActor(r.Context(), r.PathValue("username"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	pubPEM, err := h.app.FederationKeys.GetOrCreatePublicPEM(r.Context(), u.ID)
	if err != nil {
		h.logger.Error("federation: key load failed", "user", u.ID, "error", err)
		http.Error(w, "key error", http.StatusInternalServerError)
		return
	}
	id := h.actorID(u.Username)
	name := u.DisplayName
	if name == "" {
		name = u.Username
	}
	writeAP(w, map[string]any{
		"@context": []any{
			"https://www.w3.org/ns/activitystreams",
			"https://w3id.org/security/v1",
		},
		"type":              "Person",
		"id":                id,
		"preferredUsername": u.Username,
		"name":              name,
		"url":               h.base + "/u/" + u.Username,
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

func (h *fedHandlers) followers(w http.ResponseWriter, r *http.Request) {
	u, ok := h.resolveActor(r.Context(), r.PathValue("username"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	counts, _ := h.app.Follows.Counts(r.Context(), u.ID)
	writeAP(w, orderedCollection(h.actorID(u.Username)+"/followers", counts.Followers))
}

func (h *fedHandlers) following(w http.ResponseWriter, r *http.Request) {
	u, ok := h.resolveActor(r.Context(), r.PathValue("username"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	counts, _ := h.app.Follows.Counts(r.Context(), u.ID)
	writeAP(w, orderedCollection(h.actorID(u.Username)+"/following", counts.Following))
}

// Outbox: a paged OrderedCollection of the user's public activities, each
// rendered as the same Create we push to followers (Phase 3). The root
// returns metadata + a `first` page link; `?page=true[&offset=N]` returns
// an OrderedCollectionPage so a new follower's server can backfill history.
const fedOutboxPageSize = 20

func (h *fedHandlers) outbox(w http.ResponseWriter, r *http.Request) {
	u, ok := h.resolveActor(r.Context(), r.PathValue("username"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	outboxID := h.actorID(u.Username) + "/outbox"
	total, _ := h.app.Activities.CountPublicActivitiesForUser(r.Context(), u.ID)

	if r.URL.Query().Get("page") != "true" {
		writeAP(w, map[string]any{
			"@context":   "https://www.w3.org/ns/activitystreams",
			"type":       "OrderedCollection",
			"id":         outboxID,
			"totalItems": total,
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
	acts, _ := h.app.Activities.ListPublicActivitiesForUser(r.Context(), u.ID, fedOutboxPageSize, offset)
	aid := h.actorID(u.Username)
	items := make([]any, 0, len(acts))
	for _, a := range acts {
		cats := fedPublicCats(r.Context(), h.app, a)
		if !cats.Has(domain.CategorySummary) {
			continue
		}
		create := buildActivityCreate(h.base, aid, aid+"/activities/"+a.ID.String(), a, cats)
		delete(create, "@context") // embedded items inherit the page's context
		items = append(items, create)
	}
	page := map[string]any{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"type":         "OrderedCollectionPage",
		"id":           outboxID + "?page=true&offset=" + strconv.Itoa(offset),
		"partOf":       outboxID,
		"orderedItems": items,
	}
	if offset+len(acts) < total {
		page["next"] = outboxID + "?page=true&offset=" + strconv.Itoa(offset+fedOutboxPageSize)
	}
	writeAP(w, page)
}

// fedInboxMsg is a verified inbound activity addressed to a local user.
type fedInboxMsg struct {
	target        domain.User
	body          []byte
	remoteActorID string
	remoteInbox   string
	act           apActivity
}

func (h *fedHandlers) inbox(w http.ResponseWriter, r *http.Request) {
	target, ok := h.resolveActor(r.Context(), r.PathValue("username"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	remoteActorID, remoteInbox, ok := h.authenticateInbox(w, r, body)
	if !ok {
		return
	}

	var act apActivity
	if err := json.Unmarshal(body, &act); err != nil {
		http.Error(w, "bad activity", http.StatusBadRequest)
		return
	}
	// Drop at-least-once duplicates.
	if act.ID != "" && h.app.InboxDedup != nil {
		if seen, _ := h.app.InboxDedup.SeenOrMark(r.Context(), act.ID); seen {
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}
	h.dispatchInbox(w, r, fedInboxMsg{
		target: target, body: body, remoteActorID: remoteActorID, remoteInbox: remoteInbox, act: act,
	})
}

// authenticateInbox fetches the signer's actor (by keyId) and verifies the
// signature, writing the error response itself on failure.
func (h *fedHandlers) authenticateInbox(w http.ResponseWriter, r *http.Request, body []byte) (remoteActorID, remoteInbox string, ok bool) {
	keyID, ok := httpsig.KeyIDFromRequest(r)
	if !ok {
		http.Error(w, "unsigned request", http.StatusUnauthorized)
		return "", "", false
	}
	remoteActorID = strings.SplitN(keyID, "#", 2)[0]
	remoteHost := hostOf(remoteActorID)
	// A keyId with no resolvable host can't be blocked or rate-limited by
	// domain, so reject it outright rather than letting it slip past both
	// guards into the expensive fetch+verify path.
	if remoteHost == "" {
		http.Error(w, "bad actor", http.StatusBadRequest)
		return "", "", false
	}
	// Instance defederation: reject anything signed by a blocked domain
	// before we even fetch the signer.
	if domainBlocked(r.Context(), h.app, remoteHost) {
		http.Error(w, "blocked", http.StatusForbidden)
		return "", "", false
	}
	// DoS guard: bound inbound work per remote domain before the expensive
	// signer fetch + verification.
	if !h.inboxLimit.allow(remoteHost) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return "", "", false
	}
	remoteActor, pub, err := fetchRemoteActor(r.Context(), h.app, remoteActorID)
	if err != nil {
		h.logger.Warn("federation: cannot fetch signer", "actor", remoteActorID, "error", err)
		http.Error(w, "cannot verify signer", http.StatusUnauthorized)
		return "", "", false
	}
	if err := httpsig.Verify(r, body, pub); err != nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return "", "", false
	}
	return remoteActorID, remoteActor.Inbox, true
}

func (h *fedHandlers) dispatchInbox(w http.ResponseWriter, r *http.Request, m fedInboxMsg) {
	switch m.act.Type {
	case "Follow":
		// The Follow must target this user; auto-accept (the user opted in).
		if m.act.objectID() != h.actorID(m.target.Username) {
			http.Error(w, "wrong target", http.StatusBadRequest)
			return
		}
		h.inboxFollow(r.Context(), m)
	case "Accept":
		// A remote accepted OUR outbound Follow → mark it accepted.
		if err := h.app.FederationFollows.MarkAccepted(r.Context(), m.target.ID, m.remoteActorID, domain.FederationFollowOutbound); err != nil {
			h.logger.Warn("federation: mark outbound accepted", "error", err)
		}
	case "Create":
		h.inboxCreate(r.Context(), m)
	case "Like":
		h.inboxLike(r.Context(), m)
	case "Undo":
		h.inboxUndo(r.Context(), m)
	case "Delete":
		h.inboxDelete(r.Context(), m)
	default:
		// Like / Announce / etc. — accepted, handled in later phases.
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *fedHandlers) inboxFollow(ctx context.Context, m fedInboxMsg) {
	if err := h.app.FederationFollows.Upsert(ctx, domain.FederationFollow{
		LocalUserID: m.target.ID, RemoteActorID: m.remoteActorID,
		Direction: domain.FederationFollowInbound, Status: domain.FederationFollowAccepted,
		FollowActivityID: m.act.ID,
	}); err != nil {
		h.logger.Error("federation: store inbound follow", "error", err)
	}
	// Reply with a signed Accept, delivered to the follower's inbox.
	go h.deliverAccept( //nolint:gosec // G118: the Accept is delivered after the request returns, on its own timeout
		m.target.ID, m.act.ID, h.actorID(m.target.Username), m.body, m.remoteInbox)
}

// deliverAccept runs detached from the request so the Accept outlives it.
func (h *fedHandlers) deliverAccept(targetID domain.UserID, followID, actID string, raw []byte, inbox string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	accept := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       actID + "#accepts/" + url.PathEscape(followID),
		"type":     "Accept",
		"actor":    actID,
		"object":   json.RawMessage(raw),
	}
	if err := deliverActivity(ctx, h.app, targetID, actID, inbox, accept); err != nil {
		h.logger.Warn("federation: deliver Accept failed", "to", inbox, "error", err)
	}
}

// fedOwnsPublic reports whether a is the target's own activity and open to the public audience.
func (h *fedHandlers) fedOwnsPublic(ctx context.Context, a domain.Activity, targetID domain.UserID) bool {
	return a.UserID == targetID && len(fedPublicCats(ctx, h.app, a)) > 0
}

// inboxCreate: a reply (Note with inReplyTo a local public activity) → comment.
// Otherwise a workout from a followed actor → home-feed item.
func (h *fedHandlers) inboxCreate(ctx context.Context, m fedInboxMsg) {
	noteID, inReplyTo, body, isReply := remoteComment(m.act.Object)
	if !isReply {
		h.inboxCreateFeed(ctx, m)
		return
	}
	aid, ok := localActivityIDFromAPURL(inReplyTo, h.host)
	if !ok {
		return
	}
	if act2, err := h.app.Activities.GetActivity(ctx, aid); err == nil && h.fedOwnsPublic(ctx, act2, m.target.ID) {
		body = truncateUTF8(body, domain.MaxCommentLength)
		if err := h.app.Engagement.AddRemoteComment(ctx, aid, m.remoteActorID, noteID, body); err != nil {
			h.logger.Warn("federation: add remote comment", "error", err)
		}
	}
}

func (h *fedHandlers) inboxCreateFeed(ctx context.Context, m fedInboxMsg) {
	// Only ingest workouts from actors this user actually follows
	// (anti-spam: strangers are dropped).
	following, _ := h.app.FederationFollows.Exists(ctx, m.target.ID, m.remoteActorID, domain.FederationFollowOutbound)
	if !following {
		return
	}
	if item, ok := feedItemFromObject(m.target.ID, m.remoteActorID, m.act.Object); ok {
		if err := h.app.FederationFeed.Insert(ctx, item); err != nil {
			h.logger.Warn("federation: store feed item", "error", err)
		}
	}
}

// inboxLike: a remote actor likes one of this user's public activities →
// record a federated kudos. Scoped to the inbox owner's own public,
// non-hidden activity (no remote likes on private workouts).
func (h *fedHandlers) inboxLike(ctx context.Context, m fedInboxMsg) {
	aid, ok := localActivityIDFromAPURL(m.act.objectID(), h.host)
	if !ok {
		return
	}
	if liked, err := h.app.Activities.GetActivity(ctx, aid); err == nil && h.fedOwnsPublic(ctx, liked, m.target.ID) {
		if err := h.app.Engagement.AddRemoteKudos(ctx, aid, m.remoteActorID, m.act.ID); err != nil {
			h.logger.Warn("federation: add remote kudos", "error", err)
		}
	}
}

func (h *fedHandlers) inboxUndo(ctx context.Context, m fedInboxMsg) {
	switch m.act.objectType() {
	case "Follow":
		// Undo{Follow}: the remote stops following our user → drop the
		// inbound edge so the delivery scheduler stops fanning out to them.
		if m.act.undoneFollowTarget() == h.actorID(m.target.Username) {
			if err := h.app.FederationFollows.Delete(ctx, m.target.ID, m.remoteActorID, domain.FederationFollowInbound); err != nil {
				h.logger.Warn("federation: undo inbound follow", "error", err)
			}
		}
	case "Like":
		// Undo{Like}: the remote un-likes → remove the federated kudos.
		if aid, ok := localActivityIDFromAPURL(m.act.undoneObject("Like"), h.host); ok {
			if err := h.app.Engagement.RemoveRemoteKudos(ctx, aid, m.remoteActorID); err != nil {
				h.logger.Warn("federation: undo remote kudos", "error", err)
			}
		}
	}
}

// inboxDelete: a remote deleting its own object/actor. Scoped to the signer so a
// remote can only remove its own content from this user's view.
func (h *fedHandlers) inboxDelete(ctx context.Context, m fedInboxMsg) {
	switch objID := m.act.objectID(); objID {
	case "":
		// nothing addressable
	case m.remoteActorID:
		// Actor self-Delete (leaving the network): drop their feed items
		// and both follow edges with this user.
		if err := h.app.FederationFeed.DeleteAllFromActor(ctx, m.target.ID, m.remoteActorID); err != nil {
			h.logger.Warn("federation: delete actor feed items", "error", err)
		}
		_ = h.app.Engagement.DeleteRemoteKudosFromActor(ctx, m.target.ID, m.remoteActorID)
		_ = h.app.Engagement.DeleteRemoteCommentsFromActor(ctx, m.target.ID, m.remoteActorID)
		_ = h.app.FederationFollows.Delete(ctx, m.target.ID, m.remoteActorID, domain.FederationFollowInbound)
		_ = h.app.FederationFollows.Delete(ctx, m.target.ID, m.remoteActorID, domain.FederationFollowOutbound)
	default:
		// Delete{object}: remove that object from this user's view — it
		// may be a feed activity or a comment Note (both scoped to the
		// signer; whichever matches is removed, the other is a no-op).
		if err := h.app.FederationFeed.DeleteItem(ctx, m.target.ID, m.remoteActorID, objID); err != nil {
			h.logger.Warn("federation: delete feed item", "error", err)
		}
		if err := h.app.Engagement.DeleteRemoteComment(ctx, m.remoteActorID, objID); err != nil {
			h.logger.Warn("federation: delete remote comment", "error", err)
		}
	}
}

func (h *fedHandlers) setOptIn(w http.ResponseWriter, r *http.Request) {
	uid, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.app.Users.SetFederationEnabled(r.Context(), uid, body.Enabled); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	// Generate the keypair eagerly on enable so the actor is immediately complete.
	if body.Enabled {
		if _, err := h.app.FederationKeys.GetOrCreatePublicPEM(r.Context(), uid); err != nil {
			h.logger.Error("federation: key gen on enable failed", "user", uid, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": body.Enabled})
}

// followRemote resolves a user@host handle via WebFinger and sends a signed Follow so the
// remote actor's future activities are delivered to this user's inbox.
func (h *fedHandlers) followRemote(w http.ResponseWriter, r *http.Request) {
	uid, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Handle string `json:"handle"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Handle) == "" {
		http.Error(w, "handle required", http.StatusBadRequest)
		return
	}
	if enabled, _ := h.app.Users.IsFederationEnabled(r.Context(), uid); !enabled {
		http.Error(w, "enable federation on your profile first", http.StatusConflict)
		return
	}
	me, err := h.app.Users.GetUser(r.Context(), uid)
	if err != nil {
		http.Error(w, "user error", http.StatusInternalServerError)
		return
	}
	actorURL, err := resolveRemoteHandle(r.Context(), body.Handle)
	if err != nil {
		http.Error(w, "could not resolve "+body.Handle, http.StatusNotFound)
		return
	}
	remoteActor, _, err := fetchRemoteActor(r.Context(), h.app, actorURL)
	if err != nil {
		http.Error(w, "could not fetch remote actor", http.StatusBadGateway)
		return
	}
	_ = h.app.FederationFollows.Upsert(r.Context(), domain.FederationFollow{
		LocalUserID: uid, RemoteActorID: actorURL,
		Direction: domain.FederationFollowOutbound, Status: domain.FederationFollowPending,
	})
	myActor := h.actorID(me.Username)
	follow := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       myActor + "#follows/" + url.PathEscape(actorURL),
		"type":     "Follow", "actor": myActor, "object": actorURL,
	}
	if err := deliverActivity(r.Context(), h.app, uid, myActor, remoteActor.Inbox, follow); err != nil {
		h.logger.Warn("federation: deliver Follow failed", "to", remoteActor.Inbox, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"following": actorURL, "pending": true})
}

func (h *fedHandlers) listBlocks(w http.ResponseWriter, r *http.Request) {
	if _, ok := resolveAdminUser(r, h.app); !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	list, err := h.app.FedBlocks.List(r.Context())
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, b := range list {
		out = append(out, map[string]any{"domain": b.Domain, "reason": b.Reason, "created_at": b.CreatedAt.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": out})
}

func (h *fedHandlers) addBlock(w http.ResponseWriter, r *http.Request) {
	admin, ok := resolveAdminUser(r, h.app)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Domain string `json:"domain"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Domain) == "" {
		http.Error(w, "domain required", http.StatusBadRequest)
		return
	}
	d, err := h.app.FedBlocks.Block(r.Context(), restActor(r, admin), body.Domain, body.Reason)
	if err != nil {
		writeActionError(w, err, "block failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"domain": d, "blocked": true})
}

func (h *fedHandlers) removeBlock(w http.ResponseWriter, r *http.Request) {
	admin, ok := resolveAdminUser(r, h.app)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if err := h.app.FedBlocks.Unblock(r.Context(), restActor(r, admin), r.PathValue("domain")); err != nil {
		writeActionError(w, err, "unblock failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// orderedCollection is a minimal AS2 OrderedCollection carrying a totalItems
// count without inlining items (a first-page link comes with paging, later).
func orderedCollection(id string, total int) map[string]any {
	return map[string]any{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"type":         "OrderedCollection",
		"id":           id,
		"totalItems":   total,
		"orderedItems": []any{},
	}
}

// parseAcct extracts the username from "acct:username@host", requiring the host
// to match this instance. Returns ok=false on any mismatch.
func parseAcct(resource, host string) (string, bool) {
	resource = strings.TrimSpace(resource)
	resource = strings.TrimPrefix(resource, "acct:")
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

// writeAP writes an ActivityPub document with the correct content type.
func writeAP(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", apContentType)
	_ = json.NewEncoder(w).Encode(body)
}
