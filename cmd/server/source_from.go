package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/pipeline"
)

// mountSourceFromEndpoint registers "source data from another activity":
//
//	POST /api/activities/{id}/source-from   {donor_id, groups: [...]}
//
// The classic case: "rode the route but forgot my watch" — a manual activity
// borrows a route from a donor. pipeline.BorrowFrom does the work.
func mountSourceFromEndpoint(mux *http.ServeMux, app *App, logger *slog.Logger) {
	h := &sourceFromAPI{app: app, logger: logger}
	mux.HandleFunc("POST /api/activities/{id}/source-from", h.handle)

	logger.Info("source-from endpoint mounted", "path", "/api/activities/{id}/source-from")
}

// sourceFromAPI holds the dependencies of the source-from handler.
type sourceFromAPI struct {
	app    *App
	logger *slog.Logger
}

func (h *sourceFromAPI) handle(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	tid, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid activity id", http.StatusBadRequest)
		return
	}
	var body struct {
		DonorID string   `json:"donor_id"`
		Groups  []string `json:"groups"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	did, err := uuid.Parse(body.DonorID)
	if err != nil {
		http.Error(w, "invalid donor_id", http.StatusBadRequest)
		return
	}
	in := pipeline.BorrowInput{UserID: userID, TargetID: domain.ActivityID(tid), DonorID: domain.ActivityID(did)}
	for _, g := range body.Groups {
		in.Groups = append(in.Groups, domain.FieldGroup(g))
	}
	srcID, err := h.app.Pipeline.BorrowFrom(r.Context(), in)
	if errors.Is(err, domain.ErrNotFound) {
		http.Error(w, "activity not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.logger.Warn("source-from failed", "activity_id", in.TargetID, "error", err)
		writeActionError(w, err, "source-from failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source_id": srcID.String()})
}
