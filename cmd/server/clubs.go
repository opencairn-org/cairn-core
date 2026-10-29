package main

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

var clubSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify turns a club name into a URL-safe slug.
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = clubSlugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// clubsAPI holds the dependencies of the club endpoints.
type clubsAPI struct {
	app    *App
	logger *slog.Logger
}

// mountClubs wires clubs / groups / teams (multi-user v1).
//
//	POST   /api/clubs                  create a club
//	GET    /api/clubs                  list public + my clubs
//	GET    /api/clubs/{slug}           club detail (+ my membership)
//	POST   /api/clubs/{slug}/join      join (public clubs)
//	DELETE /api/clubs/{slug}/leave     leave (non-owner)
//	GET    /api/clubs/{slug}/members   member list
//	GET    /api/clubs/{slug}/feed      member activity feed
func mountClubs(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Clubs == nil {
		return
	}
	h := &clubsAPI{app: app, logger: logger}
	mux.HandleFunc("POST /api/clubs", h.create)
	mux.HandleFunc("GET /api/clubs", h.list)
	mux.HandleFunc("GET /api/clubs/{slug}", h.detail)
	mux.HandleFunc("POST /api/clubs/{slug}/join", h.join)
	mux.HandleFunc("DELETE /api/clubs/{slug}/leave", h.leave)
	mux.HandleFunc("GET /api/clubs/{slug}/members", h.members)
	mux.HandleFunc("GET /api/clubs/{slug}/feed", h.feed)

	logger.Info("club endpoints mounted")
}

// resolveClub loads a club by slug, returning false (404 written) on miss.
func (h *clubsAPI) resolveClub(w http.ResponseWriter, r *http.Request) (domain.Club, bool) {
	c, err := h.app.Clubs.GetBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		http.Error(w, "club not found", http.StatusNotFound)
		return domain.Club{}, false
	}
	return c, true
}

// clubsCreateBody is the POST /api/clubs request body.
type clubsCreateBody struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	IsPublic    *bool  `json:"is_public"`
}

// clubsNormalizeCreate trims and caps the create body; returns an error message on invalid input.
func clubsNormalizeCreate(body clubsCreateBody) (name, slug, desc string, isPublic bool, errMsg string) {
	name = strings.TrimSpace(body.Name)
	if name == "" {
		return "", "", "", false, "name required"
	}
	if len(name) > domain.MaxClubNameLength {
		name = name[:domain.MaxClubNameLength]
	}
	slug = slugify(name)
	if slug == "" {
		return "", "", "", false, "name must contain letters or digits"
	}
	desc = strings.TrimSpace(body.Description)
	if len(desc) > domain.MaxClubDescriptionLength {
		desc = desc[:domain.MaxClubDescriptionLength]
	}
	isPublic = true
	if body.IsPublic != nil {
		isPublic = *body.IsPublic
	}
	return name, slug, desc, isPublic, ""
}

func (h *clubsAPI) create(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var body clubsCreateBody
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	name, slug, desc, isPublic, errMsg := clubsNormalizeCreate(body)
	if errMsg != "" {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}
	// Disambiguate slug collisions by suffixing the user's short id.
	if _, err := h.app.Clubs.GetBySlug(r.Context(), slug); err == nil {
		slug = slug + "-" + me.String()[:8]
	}
	id, err := h.app.Clubs.Create(r.Context(), domain.Club{
		Slug: slug, Name: name, Description: desc, OwnerID: me, IsPublic: isPublic,
	})
	if err != nil {
		h.logger.Error("create club failed", "error", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id.String(), "slug": slug})
}

func (h *clubsAPI) list(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	cards, err := h.app.ClubViews.List(r.Context(), me, parseIntQuery(r, "limit", 50), parseIntQuery(r, "offset", 0))
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	out := make([]map[string]any, 0, len(cards))
	for _, c := range cards {
		out = append(out, clubCardJSON(c.Club, c.Members))
	}
	writeJSON(w, http.StatusOK, map[string]any{"clubs": out})
}

// writeClubError maps a club read failure to its REST status.
func writeClubError(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		http.Error(w, "club not found", http.StatusNotFound)
		return
	}
	writeActionError(w, err, "load failed")
}

func (h *clubsAPI) detail(w http.ResponseWriter, r *http.Request) {
	var viewer *domain.UserID
	if me, ok := resolveSessionUser(r, h.app); ok {
		viewer = &me
	}
	d, err := h.app.ClubViews.Get(r.Context(), r.PathValue("slug"), viewer)
	if err != nil {
		writeClubError(w, err)
		return
	}
	body := clubCardJSON(d.Club, d.Members)
	body["description"] = d.Club.Description
	body["is_member"] = d.IsMember
	body["my_role"] = string(d.Role)
	writeJSON(w, http.StatusOK, body)
}

func (h *clubsAPI) join(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	c, ok := h.resolveClub(w, r)
	if !ok {
		return
	}
	if !c.IsPublic {
		http.Error(w, "this club is invite-only", http.StatusForbidden)
		return
	}
	if err := h.app.Clubs.AddMember(r.Context(), c.ID, me, domain.ClubRoleMember); err != nil {
		http.Error(w, "join failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"is_member": true})
}

func (h *clubsAPI) leave(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	c, ok := h.resolveClub(w, r)
	if !ok {
		return
	}
	if c.OwnerID == me {
		http.Error(w, "the owner cannot leave their own club", http.StatusBadRequest)
		return
	}
	if err := h.app.Clubs.RemoveMember(r.Context(), c.ID, me); err != nil {
		http.Error(w, "leave failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"is_member": false})
}

func (h *clubsAPI) members(w http.ResponseWriter, r *http.Request) {
	var viewer *domain.UserID
	if me, ok := resolveSessionUser(r, h.app); ok {
		viewer = &me
	}
	rows, err := h.app.ClubViews.Members(r.Context(), r.PathValue("slug"), viewer, 0, 0)
	if err != nil {
		writeClubError(w, err)
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

func (h *clubsAPI) feed(w http.ResponseWriter, r *http.Request) {
	var viewer *domain.UserID
	if me, ok := resolveSessionUser(r, h.app); ok {
		viewer = &me
	}
	items, err := h.app.ClubViews.Feed(r.Context(), r.PathValue("slug"), viewer, parseIntQuery(r, "limit", 30), parseIntQuery(r, "offset", 0))
	if err != nil {
		writeClubError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, projectedFeedJSON(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"activities": out})
}

func clubCardJSON(c domain.Club, memberCount int) map[string]any {
	return map[string]any{
		"id":           c.ID.String(),
		"slug":         c.Slug,
		"name":         c.Name,
		"is_public":    c.IsPublic,
		"member_count": memberCount,
		"created_at":   c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}
