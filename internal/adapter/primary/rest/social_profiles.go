package rest

import (
	"errors"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/social"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// ProfileHandlers serve public athlete profiles and the owner's profile and
// visibility settings (multi-user v1).
//
//	GET     /api/profiles/{username}          public profile (session optional)
//	PUT     /api/profile/public               owner: toggle profile_is_public
//	GET/PUT /api/profile/visibility           owner: default visibility policy
//	GET/POST/DELETE /api/profile/privacy-zones  owner: privacy zones
type ProfileHandlers struct {
	Auth     *Auth
	Profiles *view.Profiles
	Privacy  *social.Privacy
}

// Mount registers the profile routes.
func (h *ProfileHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/profiles/{username}", h.getPublic)
	mux.HandleFunc("PUT /api/profile/public", h.setPublic)
}

// MountVisibility registers the default-policy editor and privacy-zone CRUD (#83).
func (h *ProfileHandlers) MountVisibility(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/profile/visibility", h.getVisibility)
	mux.HandleFunc("PUT /api/profile/visibility", h.putVisibility)
	mux.HandleFunc("GET /api/profile/privacy-zones", h.listZones)
	mux.HandleFunc("POST /api/profile/privacy-zones", h.addZone)
	mux.HandleFunc("DELETE /api/profile/privacy-zones/{id}", h.deleteZone)
}

func (h *ProfileHandlers) getPublic(w http.ResponseWriter, r *http.Request) {
	pv, err := h.Profiles.Get(r.Context(), r.PathValue("username"), h.Auth.Viewer(r))
	if err != nil {
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

func (h *ProfileHandlers) setPublic(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Public bool `json:"public"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.Privacy.SetProfilePublic(r.Context(), me, body.Public); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"public": body.Public})
}

func (h *ProfileHandlers) getVisibility(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	p, err := h.Privacy.Policy(r.Context(), me)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"policy":         domain.MarshalPolicy(p),
		"all_categories": socialCategoryStrings(),
		"all_audiences":  []string{"public", "followers", "link"},
	})
}

func (h *ProfileHandlers) putVisibility(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
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
	if err := h.Privacy.SetPolicy(r.Context(), me, p); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": domain.MarshalPolicy(p)})
}

func (h *ProfileHandlers) listZones(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	zones, err := h.Privacy.Zones(r.Context(), me)
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

func (h *ProfileHandlers) addZone(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
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
	id, err := h.Privacy.AddZone(r.Context(), me, domain.PrivacyZone{
		Label: body.Label, Lat: body.Lat, Lng: body.Lng, RadiusM: body.RadiusM,
	})
	if err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id.String()})
}

func (h *ProfileHandlers) deleteZone(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.PrivacyZoneID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.Privacy.DeleteZone(r.Context(), me, id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func socialCategoryStrings() []string {
	out := make([]string, len(domain.AllCategories))
	for i, c := range domain.AllCategories {
		out[i] = string(c)
	}
	return out
}
