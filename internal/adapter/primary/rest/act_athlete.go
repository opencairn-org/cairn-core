package rest

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/athlete"
)

// AthleteHandlers expose the athlete physiology profile: dated FTP, weight,
// threshold HR, … values that feed the TSS estimate.
//
//	GET    /api/athlete/metrics              { specs:[...], entries:[...] }
//	PUT    /api/athlete/metrics              { key, effective_date, value }
//	DELETE /api/athlete/metrics/{id}
//	POST   /api/athlete/recalculate          re-derive TSS + training load
type AthleteHandlers struct {
	Auth    *Auth
	Profile *athlete.Profile
}

// Mount registers the routes.
func (h *AthleteHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/athlete/metrics", h.list)
	mux.HandleFunc("PUT /api/athlete/metrics", h.put)
	mux.HandleFunc("DELETE /api/athlete/metrics/{id}", h.remove)
	mux.HandleFunc("POST /api/athlete/recalculate", h.recalculate)
}

// list returns the metric registry (for the UI form) plus the user's entries.
func (h *AthleteHandlers) list(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	entries, err := h.Profile.Entries(r.Context(), me)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	specs := make([]map[string]any, 0, len(domain.AthleteMetricSpecs))
	for _, s := range domain.AthleteMetricSpecs {
		specs = append(specs, map[string]any{
			"key": string(s.Key), "label": s.Label, "unit": s.Unit, "min": s.Min, "max": s.Max,
		})
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"id":             e.ID.String(),
			"key":            string(e.Key),
			"effective_date": e.EffectiveDate.UTC().Format("2006-01-02"),
			"value":          e.Value,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"specs": specs, "entries": out})
}

// put upserts one dated measurement.
func (h *AthleteHandlers) put(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Key           string  `json:"key"`
		EffectiveDate string  `json:"effective_date"`
		Value         float64 `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	day, err := time.Parse("2006-01-02", body.EffectiveDate)
	if err != nil {
		http.Error(w, "effective_date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	id, err := h.Profile.Put(r.Context(), domain.AthleteMetricEntry{
		UserID: me, Key: domain.AthleteMetricKey(body.Key), EffectiveDate: day.UTC(), Value: body.Value,
	})
	if err != nil {
		writeActionError(w, err, "save failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id.String()})
}

func (h *AthleteHandlers) remove(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.AthleteMetricID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.Profile.Delete(r.Context(), me, id); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// recalculate re-derives TSS under the current profile so a profile edit
// corrects the CTL/ATL/TSB history.
func (h *AthleteHandlers) recalculate(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	res, err := h.Profile.Recalculate(r.Context(), me)
	if errors.Is(err, athlete.ErrRecomputeUnavailable) {
		http.Error(w, "recompute unavailable", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recomputed": res.Recomputed, "training_load_days": res.TrainingLoadDays})
}
