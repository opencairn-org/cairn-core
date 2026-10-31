package rest

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	"github.com/opencairn-org/cairn-core/internal/usecase/instance"
	matchuc "github.com/opencairn-org/cairn-core/internal/usecase/match"
)

// MergeOverrideHandlers are the manual merge overrides. Field pins force which
// source wins a field group; merge/split add must/cannot-link constraints and
// recluster.
//
//	GET    /api/activities/{id}/field-sources          list pins
//	PUT    /api/activities/{id}/field-source           { field, source_id }
//	DELETE /api/activities/{id}/field-source/{field}
//	POST   /api/activities/merge                       { activity_a, activity_b } (must-link)
//	POST   /api/activities/{id}/split                  { source_id } (cannot-link)
type MergeOverrideHandlers struct {
	Auth    *Auth
	Actions *activity.SourceActions
	Pins    *activity.FieldPins
	// Links is the re-clustering engine (nil → merge/split unavailable).
	Links *matchuc.SourceLinks
	Log   *slog.Logger
}

// Mount registers the routes.
func (h *MergeOverrideHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/activities/{id}/field-sources", h.listFieldSources)
	mux.HandleFunc("PUT /api/activities/{id}/field-source", h.setFieldSource)
	mux.HandleFunc("DELETE /api/activities/{id}/field-source/{field}", h.deleteFieldSource)
	mux.HandleFunc("POST /api/activities/merge", h.merge)
	mux.HandleFunc("POST /api/activities/{id}/split", h.split)
}

// actValidFieldGroup reports whether fg is a real, user-pinnable field group.
func actValidFieldGroup(fg domain.FieldGroup) bool {
	return slices.Contains(domain.AllFieldGroups, fg)
}

func (h *MergeOverrideHandlers) listFieldSources(w http.ResponseWriter, r *http.Request) {
	act, ok := actOwnedActivity(w, r, h.Auth, h.Actions)
	if !ok {
		return
	}
	pins, err := h.Pins.List(r.Context(), act)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]string, 0, len(pins))
	for _, p := range pins {
		out = append(out, map[string]string{"field": string(p.FieldKey), "source_id": p.SourceID.String()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"overrides": out})
}

func (h *MergeOverrideHandlers) setFieldSource(w http.ResponseWriter, r *http.Request) {
	act, ok := actOwnedActivity(w, r, h.Auth, h.Actions)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Field    string `json:"field"`
		SourceID string `json:"source_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	fg := domain.FieldGroup(body.Field)
	if !actValidFieldGroup(fg) {
		http.Error(w, "unknown field group", http.StatusBadRequest)
		return
	}
	srcID, err := domain.ParseUUID[domain.SourceID](body.SourceID)
	if err != nil {
		http.Error(w, "bad source id", http.StatusBadRequest)
		return
	}
	if err := h.Pins.Set(r.Context(), act, fg, srcID); err != nil {
		if errors.Is(err, activity.ErrPinSourceNotOnActivity) {
			http.Error(w, "source not on this activity", http.StatusBadRequest)
			return
		}
		http.Error(w, "set failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "field": body.Field, "source_id": body.SourceID})
}

func (h *MergeOverrideHandlers) deleteFieldSource(w http.ResponseWriter, r *http.Request) {
	act, ok := actOwnedActivity(w, r, h.Auth, h.Actions)
	if !ok {
		return
	}
	fg := domain.FieldGroup(r.PathValue("field"))
	if !actValidFieldGroup(fg) {
		http.Error(w, "unknown field group", http.StatusBadRequest)
		return
	}
	if err := h.Pins.Clear(r.Context(), act, fg); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *MergeOverrideHandlers) merge(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	if h.Links == nil {
		http.Error(w, "re-clustering engine unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		ActivityA string `json:"activity_a"`
		ActivityB string `json:"activity_b"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	a, errA := domain.ParseUUID[domain.ActivityID](body.ActivityA)
	b, errB := domain.ParseUUID[domain.ActivityID](body.ActivityB)
	if errA != nil || errB != nil || a == b {
		http.Error(w, "two distinct activity ids required", http.StatusBadRequest)
		return
	}
	if err := h.Links.LinkActivities(r.Context(), me, a, b); err != nil {
		h.writeLinkError(w, "merge recorded but recluster failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"merged": true})
}

// writeLinkError maps a SourceLinks failure to its HTTP status.
func (h *MergeOverrideHandlers) writeLinkError(w http.ResponseWriter, reclusterMsg string, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, matchuc.ErrSourceNotOnActivity):
		http.Error(w, "source not on this activity", http.StatusBadRequest)
	case errors.Is(err, matchuc.ErrSingleSource):
		http.Error(w, "activity has only one source — nothing to split from", http.StatusConflict)
	case errors.Is(err, matchuc.ErrReclusterFailed):
		h.Log.Warn(reclusterMsg, "error", err)
		http.Error(w, reclusterMsg, http.StatusInternalServerError)
	default:
		h.Log.Error("source link failed", "error", err)
		http.Error(w, "link failed", http.StatusInternalServerError)
	}
}

// split peels one source off into its own activity.
func (h *MergeOverrideHandlers) split(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	aID, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad activity id", http.StatusBadRequest)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	if h.Links == nil {
		http.Error(w, "re-clustering engine unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		SourceID string `json:"source_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	peel, err := domain.ParseUUID[domain.SourceID](body.SourceID)
	if err != nil {
		http.Error(w, "bad source id", http.StatusBadRequest)
		return
	}
	if _, err := h.Links.Split(r.Context(), me, aID, peel); err != nil {
		h.writeLinkError(w, "split recorded but recluster failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"split": true})
}

// MergePolicyHandlers configure the priority cascade (per-user → instance
// default → AnyProvider; per-field pins beat all). A policy is
// {default_priority:[...], overrides:{field:[...]}}.
//
//	GET/PUT /api/settings/merge-policy             user
//	GET/PUT /api/admin/instance/merge-defaults     admin
type MergePolicyHandlers struct {
	Auth     *Auth
	Policies *instance.MergePolicies
	Defaults *instance.MergeDefaults
	Log      *slog.Logger
}

// Mount registers the routes.
func (h *MergePolicyHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/settings/merge-policy", h.getUser)
	mux.HandleFunc("PUT /api/settings/merge-policy", h.putUser)
	mux.HandleFunc("GET /api/admin/instance/merge-defaults", h.getInstance)
	mux.HandleFunc("PUT /api/admin/instance/merge-defaults", h.putInstance)
}

func (h *MergePolicyHandlers) getUser(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	policies, defaults, err := h.Policies.WithDefaults(r.Context(), me)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	groups := make([]string, 0, len(domain.AllFieldGroups))
	for _, g := range domain.AllFieldGroups {
		groups = append(groups, string(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"policies":          actPoliciesJSON(policies),
		"instance_defaults": actPoliciesJSON(defaults),
		"field_groups":      groups,
	})
}

func (h *MergePolicyHandlers) putUser(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	policies, ok := actDecodePolicies(w, r)
	if !ok {
		return
	}
	if err := h.Policies.Set(r.Context(), me, policies); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	h.Log.Info("user merge policy updated", "user_id", me, "types", len(policies))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *MergePolicyHandlers) getInstance(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.Auth.Admin(r); !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	defaults, groups, err := h.Defaults.Get(r.Context())
	if err != nil {
		writeActionError(w, err, "load failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": actPoliciesJSON(defaults), "field_groups": groups})
}

func (h *MergePolicyHandlers) putInstance(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.Auth.Admin(r)
	if !ok {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	policies, ok := actDecodePolicies(w, r)
	if !ok {
		return
	}
	if err := h.Defaults.Set(r.Context(), actor(r, admin), policies); err != nil {
		writeActionError(w, err, "save failed")
		return
	}
	h.Log.Info("instance merge defaults updated", "types", len(policies))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// actPolicyJSON is the wire shape of one merge policy.
type actPolicyJSON struct {
	DefaultPriority []string            `json:"default_priority"`
	Overrides       map[string][]string `json:"overrides,omitempty"`
}

func actPoliciesJSON(m map[domain.ActivityType]domain.MergePolicy) map[string]actPolicyJSON {
	out := make(map[string]actPolicyJSON, len(m))
	for t, p := range m {
		wire := actPolicyJSON{DefaultPriority: p.DefaultPriority}
		if len(p.Overrides) > 0 {
			wire.Overrides = make(map[string][]string, len(p.Overrides))
			for k, v := range p.Overrides {
				wire.Overrides[string(k)] = v
			}
		}
		out[string(t)] = wire
	}
	return out
}

// actDecodePolicies reads {policies:{type:policy}}, writing 400 on a bad body.
func actDecodePolicies(w http.ResponseWriter, r *http.Request) (map[domain.ActivityType]domain.MergePolicy, bool) {
	var body struct {
		Policies map[string]actPolicyJSON `json:"policies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return nil, false
	}
	out := make(map[domain.ActivityType]domain.MergePolicy, len(body.Policies))
	for t, wire := range body.Policies {
		mp := domain.MergePolicy{DefaultPriority: wire.DefaultPriority}
		if len(wire.Overrides) > 0 {
			mp.Overrides = make(map[domain.FieldGroup][]string, len(wire.Overrides))
			for k, v := range wire.Overrides {
				fg := domain.FieldGroup(k)
				if !actValidFieldGroup(fg) {
					http.Error(w, "unknown field group: "+k, http.StatusBadRequest)
					return nil, false
				}
				mp.Overrides[fg] = v
			}
		}
		out[domain.ActivityType(t)] = mp
	}
	return out, true
}
