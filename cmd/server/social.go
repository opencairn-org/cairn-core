package main

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// mountSocial wires the follow-graph endpoints (multi-user v1).
//
//	POST   /api/users/{id}/follow      follow the user
//	DELETE /api/users/{id}/follow      unfollow
//	GET    /api/users/{id}/follow-state  { following, followers, following_count }
//	GET    /api/users/{id}/followers   [{id,username,display_name,avatar_url}]
//	GET    /api/users/{id}/following
func mountSocial(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Follows == nil {
		return
	}
	h := &socialAPI{app: app, logger: logger}
	mux.HandleFunc("POST /api/users/{id}/follow", h.follow)
	mux.HandleFunc("DELETE /api/users/{id}/follow", h.unfollow)
	mux.HandleFunc("GET /api/users/{id}/follow-state", h.followState)
	mux.HandleFunc("GET /api/users/{id}/followers", h.listHandler("followers"))
	mux.HandleFunc("GET /api/users/{id}/following", h.listHandler("following"))
	// Following feed: reverse-chronological activities from people you follow.
	mux.HandleFunc("GET /api/feed/following", h.followingFeed)
	// Projected single-activity read for non-owners (followers / public).
	// The owner's own rich view goes through Connect-RPC (owner-only); this is
	// the gated cross-user path that the feed/profile cards link to.
	mux.HandleFunc("GET /api/activities/{id}/projected", h.projected)

	logger.Info("social (follow-graph) endpoints mounted")
}

// socialAPI holds the dependencies of the follow-graph handlers.
type socialAPI struct {
	app    *App
	logger *slog.Logger
}

// meAndTarget resolves the session user and the {id} target or writes an error.
func (h *socialAPI) meAndTarget(w http.ResponseWriter, r *http.Request) (domain.UserID, domain.UserID, bool) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return me, domain.UserID{}, false
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad user id", http.StatusBadRequest)
		return me, domain.UserID{}, false
	}
	return me, domain.UserID(id), true
}

func (h *socialAPI) follow(w http.ResponseWriter, r *http.Request) {
	app := h.app
	me, target, ok := h.meAndTarget(w, r)
	if !ok {
		return
	}
	if me == target {
		http.Error(w, "cannot follow yourself", http.StatusBadRequest)
		return
	}
	if app.Blocks != nil {
		if blocked, _ := app.Blocks.IsBlockedEitherWay(r.Context(), me, target); blocked {
			http.Error(w, "cannot follow this user", http.StatusForbidden)
			return
		}
	}
	if err := app.Follows.Follow(r.Context(), me, target); err != nil {
		h.logger.Error("follow failed", "error", err)
		http.Error(w, "follow failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"following": true})
}

func (h *socialAPI) unfollow(w http.ResponseWriter, r *http.Request) {
	me, target, ok := h.meAndTarget(w, r)
	if !ok {
		return
	}
	if err := h.app.Follows.Unfollow(r.Context(), me, target); err != nil {
		http.Error(w, "unfollow failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"following": false})
}

func (h *socialAPI) followState(w http.ResponseWriter, r *http.Request) {
	me, target, ok := h.meAndTarget(w, r)
	if !ok {
		return
	}
	st, err := h.app.ProfileViews.FollowState(r.Context(), me, target)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"following":       st.Following,
		"followers_count": st.Counts.Followers,
		"following_count": st.Counts.Following,
		"is_self":         st.IsSelf,
	})
}

// listHandler serves the followers ("followers") or following list of a user.
func (h *socialAPI) listHandler(which string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, target, ok := h.meAndTarget(w, r)
		if !ok {
			return
		}
		limit := parseIntQuery(r, "limit", 50)
		offset := parseIntQuery(r, "offset", 0)
		list := h.app.ProfileViews.Following
		if which == "followers" {
			list = h.app.ProfileViews.Followers
		}
		users, err := list(r.Context(), target, limit, offset)
		if err != nil {
			http.Error(w, "load failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": userCardsJSON(users)})
	}
}

func (h *socialAPI) followingFeed(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	limit := parseIntQuery(r, "limit", view.FeedDefaultLimit)
	if limit > view.FeedMaxLimit {
		limit = view.FeedMaxLimit
	}
	offset := parseIntQuery(r, "offset", 0)
	items := h.app.Feeds.Following(r.Context(), me, limit, offset)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, projectedFeedJSON(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activities": out,
		"offset":     offset,
		"limit":      limit,
		// has_more reflects whether the DB returned a full page, independent
		// of how many survived the visibility filter.
		"has_more": len(items) == limit,
	})
}

// projectedFeedJSON renders a projected (or own) feed row with its owner card.
func projectedFeedJSON(it view.FeedItem) map[string]any {
	var m map[string]any
	if it.Own != nil {
		m = projectActivityJSON(*it.Own, domain.ResolveVisibility(domain.VisibilityInput{IsOwner: true}), nil)
	} else {
		m = projectedJSON(*it.Projected)
	}
	m["owner"] = userCardJSON(it.Owner)
	return m
}

func (h *socialAPI) projected(w http.ResponseWriter, r *http.Request) {
	actID, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var viewerPtr *domain.UserID
	if v, ok := optionalSessionUser(r, h.app); ok {
		viewerPtr = &v
	}
	p, owner, isOwner, err := h.app.Feeds.Projected(r.Context(), actID, viewerPtr, false)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activity": projectedJSON(p),
		"owner":    userCardJSON(owner),
		"is_owner": isOwner,
	})
}

// userCardJSON is the compact public-facing shape of a user in social lists.
// userCardsJSON renders a list of user cards.
func userCardsJSON(users []domain.User) []map[string]any {
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, userCardJSON(u))
	}
	return out
}

func userCardJSON(u domain.User) map[string]any {
	return map[string]any{
		"id":           u.ID.String(),
		"username":     u.Username,
		"display_name": u.DisplayName,
		"avatar_url":   u.AvatarURL,
	}
}
