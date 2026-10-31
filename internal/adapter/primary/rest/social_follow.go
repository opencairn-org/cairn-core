package rest

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/social"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// FollowHandlers serve the follow graph, the following feed and the projected
// single-activity read (multi-user v1).
//
//	POST   /api/users/{id}/follow        follow the user
//	DELETE /api/users/{id}/follow        unfollow
//	GET    /api/users/{id}/follow-state  { following, followers_count, following_count, is_self }
//	GET    /api/users/{id}/followers     [{id,username,display_name,avatar_url}]
//	GET    /api/users/{id}/following
//	GET    /api/feed/following           projected activities of people you follow
//	GET    /api/activities/{id}/projected  gated cross-user read (session optional)
type FollowHandlers struct {
	Auth     *Auth
	Graph    *social.Graph
	Profiles *view.Profiles
	Feeds    *view.Feeds
	Log      *slog.Logger
}

// Mount registers the routes.
func (h *FollowHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/users/{id}/follow", h.follow)
	mux.HandleFunc("DELETE /api/users/{id}/follow", h.unfollow)
	mux.HandleFunc("GET /api/users/{id}/follow-state", h.followState)
	mux.HandleFunc("GET /api/users/{id}/followers", h.list(h.Profiles.Followers))
	mux.HandleFunc("GET /api/users/{id}/following", h.list(h.Profiles.Following))
	mux.HandleFunc("GET /api/feed/following", h.followingFeed)
	// The owner's rich view is Connect (owner-only); this is the gated path feed/profile cards link to.
	mux.HandleFunc("GET /api/activities/{id}/projected", h.projected)
}

// meAndTarget resolves the caller and the {id} target or writes an error.
func (h *FollowHandlers) meAndTarget(w http.ResponseWriter, r *http.Request) (domain.UserID, domain.UserID, bool) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return me, domain.UserID{}, false
	}
	id, err := domain.ParseUUID[domain.UserID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad user id", http.StatusBadRequest)
		return me, domain.UserID{}, false
	}
	return me, id, true
}

func (h *FollowHandlers) follow(w http.ResponseWriter, r *http.Request) {
	me, target, ok := h.meAndTarget(w, r)
	if !ok {
		return
	}
	if err := h.Graph.Follow(r.Context(), me, target); err != nil {
		if _, isAction := actionErrorOf(err); !isAction {
			h.Log.Error("follow failed", "error", err)
		}
		writeActionError(w, err, "follow failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"following": true})
}

func (h *FollowHandlers) unfollow(w http.ResponseWriter, r *http.Request) {
	me, target, ok := h.meAndTarget(w, r)
	if !ok {
		return
	}
	if err := h.Graph.Unfollow(r.Context(), me, target); err != nil {
		http.Error(w, "unfollow failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"following": false})
}

func (h *FollowHandlers) followState(w http.ResponseWriter, r *http.Request) {
	me, target, ok := h.meAndTarget(w, r)
	if !ok {
		return
	}
	st, err := h.Profiles.FollowState(r.Context(), me, target)
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

// socialUserList is view.Profiles.Followers or .Following.
type socialUserList func(ctx context.Context, me, target domain.UserID, limit, offset int) ([]domain.User, error)

// list serves one side of a user's follow graph.
func (h *FollowHandlers) list(load socialUserList) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, target, ok := h.meAndTarget(w, r)
		if !ok {
			return
		}
		users, err := load(r.Context(), me, target, parseIntQuery(r, "limit", 50), parseIntQuery(r, "offset", 0))
		if err != nil {
			writeActionError(w, err, "load failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": userCardsJSON(users)})
	}
}

func (h *FollowHandlers) followingFeed(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	limit := min(parseIntQuery(r, "limit", view.FeedDefaultLimit), view.FeedMaxLimit)
	offset := parseIntQuery(r, "offset", 0)
	items := h.Feeds.Following(r.Context(), me, limit, offset)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, socialProjectedFeedJSON(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activities": out,
		"offset":     offset,
		"limit":      limit,
		// A full DB page, independent of how many rows survived the visibility filter.
		"has_more": len(items) == limit,
	})
}

func (h *FollowHandlers) projected(w http.ResponseWriter, r *http.Request) {
	actID, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	p, owner, isOwner, err := h.Feeds.Projected(r.Context(), actID, h.Auth.Viewer(r), false)
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activity": projectedJSON(p),
		"owner":    userCardJSON(owner),
		"is_owner": isOwner,
	})
}

// socialProjectedFeedJSON renders a projected (or own) feed row with its owner card.
func socialProjectedFeedJSON(it view.FeedItem) map[string]any {
	var m map[string]any
	if it.Own != nil {
		m = projectActivityJSON(*it.Own, domain.ResolveVisibility(domain.VisibilityInput{IsOwner: true}), nil)
	} else {
		m = projectedJSON(*it.Projected)
	}
	m["owner"] = userCardJSON(it.Owner)
	return m
}
