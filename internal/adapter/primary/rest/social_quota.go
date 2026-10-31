package rest

import (
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/useradmin"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// QuotaHandlers serve the caller's quota usage and the admin per-user override.
//
//	GET /api/quota                   the caller's own usage + limit
//	PUT /api/admin/users/{id}/quota  admin: set/clear per-user override
type QuotaHandlers struct {
	Auth      *Auth
	Quota     *view.Quota
	UserAdmin *useradmin.Users
}

// Mount registers the routes.
func (h *QuotaHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/quota", h.get)
	mux.HandleFunc("PUT /api/admin/users/{id}/quota", h.set)
}

func (h *QuotaHandlers) get(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	// A counting failure reads as no usage so a DB hiccup never blocks an upload.
	st, _ := h.Quota.Status(r.Context(), me)
	writeJSON(w, http.StatusOK, map[string]any{
		"activities_used":  st.Used,
		"activities_limit": st.Limit,
		"unlimited":        st.Unlimited,
		"over_limit":       st.OverLimit,
	})
}

func (h *QuotaHandlers) set(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.Auth.Admin(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	target, err := domain.ParseUUID[domain.UserID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		// null clears the override (instance default); 0 is unlimited; >0 a cap.
		MaxActivities *int `json:"max_activities"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if h.UserAdmin == nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := h.UserAdmin.SetQuota(r.Context(), actor(r, admin), target, body.MaxActivities); err != nil {
		writeActionError(w, err, "save failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"max_activities": body.MaxActivities})
}
