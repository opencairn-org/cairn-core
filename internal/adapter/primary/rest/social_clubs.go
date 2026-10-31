package rest

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/social"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// ClubHandlers serve clubs / groups / teams (multi-user v1).
//
//	POST   /api/clubs                  create a club
//	GET    /api/clubs                  list public + my clubs
//	GET    /api/clubs/{slug}           club detail (+ my membership)
//	POST   /api/clubs/{slug}/join      join (public clubs)
//	DELETE /api/clubs/{slug}/leave     leave (non-owner)
//	GET    /api/clubs/{slug}/members   member list
//	GET    /api/clubs/{slug}/feed      member activity feed
type ClubHandlers struct {
	Auth  *Auth
	Clubs *social.Clubs
	Views *view.Clubs
	Log   *slog.Logger
}

// Mount registers the routes.
func (h *ClubHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/clubs", h.create)
	mux.HandleFunc("GET /api/clubs", h.list)
	mux.HandleFunc("GET /api/clubs/{slug}", h.detail)
	mux.HandleFunc("POST /api/clubs/{slug}/join", h.join)
	mux.HandleFunc("DELETE /api/clubs/{slug}/leave", h.leave)
	mux.HandleFunc("GET /api/clubs/{slug}/members", h.members)
	mux.HandleFunc("GET /api/clubs/{slug}/feed", h.feed)
}

func (h *ClubHandlers) create(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		IsPublic    *bool  `json:"is_public"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	id, slug, err := h.Clubs.Create(r.Context(), me, social.NewClub{Name: body.Name, Description: body.Description, IsPublic: body.IsPublic})
	if err != nil {
		if _, isAction := actionErrorOf(err); !isAction {
			h.Log.Error("create club failed", "error", err)
		}
		writeActionError(w, err, "create failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id.String(), "slug": slug})
}

func (h *ClubHandlers) list(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	cards, err := h.Views.List(r.Context(), me, parseIntQuery(r, "limit", 50), parseIntQuery(r, "offset", 0))
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(cards))
	for _, c := range cards {
		out = append(out, socialClubCardJSON(c.Club, c.Members))
	}
	writeJSON(w, http.StatusOK, map[string]any{"clubs": out})
}

// socialWriteClubError maps a club read failure to its REST status.
func socialWriteClubError(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		http.Error(w, "club not found", http.StatusNotFound)
		return
	}
	writeActionError(w, err, "load failed")
}

func (h *ClubHandlers) detail(w http.ResponseWriter, r *http.Request) {
	d, err := h.Views.Get(r.Context(), r.PathValue("slug"), h.Auth.Viewer(r))
	if err != nil {
		socialWriteClubError(w, err)
		return
	}
	body := socialClubCardJSON(d.Club, d.Members)
	body["description"] = d.Club.Description
	body["is_member"] = d.IsMember
	body["my_role"] = string(d.Role)
	writeJSON(w, http.StatusOK, body)
}

func (h *ClubHandlers) join(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if err := h.Clubs.Join(r.Context(), me, r.PathValue("slug")); err != nil {
		writeActionError(w, err, "join failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"is_member": true})
}

func (h *ClubHandlers) leave(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if err := h.Clubs.Leave(r.Context(), me, r.PathValue("slug")); err != nil {
		writeActionError(w, err, "leave failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"is_member": false})
}

func (h *ClubHandlers) members(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Views.Members(r.Context(), r.PathValue("slug"), h.Auth.Viewer(r), 0, 0)
	if err != nil {
		socialWriteClubError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		card := map[string]any{"role": string(m.Member.Role)}
		if m.User.ID != (domain.UserID{}) {
			for k, v := range userCardJSON(m.User) {
				card[k] = v
			}
		}
		out = append(out, card)
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

func (h *ClubHandlers) feed(w http.ResponseWriter, r *http.Request) {
	items, err := h.Views.Feed(r.Context(), r.PathValue("slug"), h.Auth.Viewer(r), parseIntQuery(r, "limit", 30), parseIntQuery(r, "offset", 0))
	if err != nil {
		socialWriteClubError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, socialProjectedFeedJSON(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"activities": out})
}

func socialClubCardJSON(c domain.Club, memberCount int) map[string]any {
	return map[string]any{
		"id":           c.ID.String(),
		"slug":         c.Slug,
		"name":         c.Name,
		"is_public":    c.IsPublic,
		"member_count": memberCount,
		"created_at":   c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}
