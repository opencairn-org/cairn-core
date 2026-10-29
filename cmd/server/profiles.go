package main

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// mountProfiles wires public athlete profiles + the owner's profile/visibility
// settings (multi-user v1).
//
//	GET    /api/profiles/{username}        public profile (session optional)
//	GET    /api/profile/settings           owner: profile + visibility settings
//	PUT    /api/profile/public             owner: toggle profile_is_public
//	GET/PUT /api/profile/visibility        owner: default visibility policy
//	GET/POST/DELETE /api/profile/privacy-zones  owner: privacy zones
func mountProfiles(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Users == nil {
		return
	}
	h := &profilesAPI{app: app}
	mux.HandleFunc("GET /api/profiles/{username}", h.getPublic)
	mux.HandleFunc("PUT /api/profile/public", h.setPublic)

	if app.Visibility != nil {
		mountVisibilitySettings(mux, app)
	}

	logger.Info("profile endpoints mounted")
}

// profilesAPI holds the dependencies of the profile + visibility handlers.
type profilesAPI struct {
	app *App
}

func (h *profilesAPI) getPublic(w http.ResponseWriter, r *http.Request) {
	var viewer *domain.UserID
	if v, ok := optionalSessionUser(r, h.app); ok {
		viewer = &v
	}
	pv, err := h.app.ProfileViews.Get(r.Context(), r.PathValue("username"), viewer)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeActionError(w, err, "load failed")
		return
	}
	acts := make([]map[string]any, 0, len(pv.Activities))
	for _, p := range pv.Activities {
		acts = append(acts, projectedJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":            userCardJSON(pv.User),
		"is_public":       pv.User.ProfileIsPublic,
		"is_self":         pv.IsOwner,
		"is_following":    pv.IsFollowing,
		"followers_count": pv.Counts.Followers,
		"following_count": pv.Counts.Following,
		"activities":      acts,
	})
}

func (h *profilesAPI) setPublic(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var body struct {
		Public bool `json:"public"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.app.Users.SetProfilePublic(r.Context(), me, body.Public); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"public": body.Public})
}

// mountVisibilitySettings wires the owner's default-visibility-policy editor
// and privacy-zone CRUD (#83).
func mountVisibilitySettings(mux *http.ServeMux, app *App) {
	h := &profilesAPI{app: app}
	mux.HandleFunc("GET /api/profile/visibility", h.getVisibility)
	mux.HandleFunc("PUT /api/profile/visibility", h.putVisibility)
	mux.HandleFunc("GET /api/profile/privacy-zones", h.listZones)
	mux.HandleFunc("POST /api/profile/privacy-zones", h.addZone)
	mux.HandleFunc("DELETE /api/profile/privacy-zones/{id}", h.deleteZone)
}

func (h *profilesAPI) getVisibility(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	p, err := h.app.Visibility.GetUserPolicy(r.Context(), me)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	if p.ByAudience == nil {
		p = domain.DefaultVisibilityPolicy()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"policy":         domain.MarshalPolicy(p),
		"all_categories": categoryStrings(),
		"all_audiences":  []string{"public", "followers", "link"},
	})
}

func (h *profilesAPI) putVisibility(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var body struct {
		Policy map[string][]string `json:"policy"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	p := domain.UnmarshalPolicy(body.Policy)
	if err := h.app.Visibility.SetUserPolicy(r.Context(), me, p); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": domain.MarshalPolicy(p)})
}

func (h *profilesAPI) listZones(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	zones, err := h.app.Visibility.ListPrivacyZones(r.Context(), me)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(zones))
	for _, z := range zones {
		out = append(out, map[string]any{
			"id": z.ID.String(), "label": z.Label,
			"lat": z.Lat, "lng": z.Lng, "radius_m": z.RadiusM,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"zones": out})
}

func (h *profilesAPI) addZone(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var body struct {
		Label   string  `json:"label"`
		Lat     float64 `json:"lat"`
		Lng     float64 `json:"lng"`
		RadiusM float64 `json:"radius_m"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if body.RadiusM <= 0 {
		body.RadiusM = 200
	}
	id, err := h.app.Visibility.AddPrivacyZone(r.Context(), domain.PrivacyZone{
		UserID: me, Label: body.Label, Lat: body.Lat, Lng: body.Lng, RadiusM: body.RadiusM,
	})
	if err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id.String()})
}

func (h *profilesAPI) deleteZone(w http.ResponseWriter, r *http.Request) {
	me, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id, err := domain.ParseUUID[domain.PrivacyZoneID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.app.Visibility.DeletePrivacyZone(r.Context(), id, me); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func categoryStrings() []string {
	out := make([]string, len(domain.AllCategories))
	for i, c := range domain.AllCategories {
		out[i] = string(c)
	}
	return out
}
