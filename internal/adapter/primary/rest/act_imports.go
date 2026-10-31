package rest

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	"github.com/opencairn-org/cairn-core/internal/usecase/pipeline"
)

// actMaxBodyBytes caps an upload body: the file plus slack for multipart framing.
const actMaxBodyBytes = port.MaxUploadBytes + 1<<20

// ImportHandlers create activities from user input. Both feed the worker
// ingest path, so the result dedups, re-merges and edits like any import.
//
//	POST /api/activities/upload   multipart "file" = .gpx/.tcx/.fit (provider "upload", content-hash id)
//	POST /api/activities/manual   JSON summary values (provider "manual", fresh id per submission)
//	POST /api/activities/{id}/source-from   { donor_id, groups: [...] }
//
// source-from is "rode the route but forgot my watch": the activity borrows
// field groups (and the stream) from a donor.
type ImportHandlers struct {
	Auth     *Auth
	Imports  *activity.Imports
	Pipeline *pipeline.Pipeline
	Log      *slog.Logger
}

// Mount registers the routes.
func (h *ImportHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/activities/upload", h.upload)
	mux.HandleFunc("POST /api/activities/manual", h.manual)
	mux.HandleFunc("POST /api/activities/{id}/source-from", h.sourceFrom)
}

// writeQuotaReached answers 403 with the usage when err is a quota refusal.
func (h *ImportHandlers) writeQuotaReached(w http.ResponseWriter, err error) bool {
	var qe *activity.QuotaReachedError
	if !errors.As(err, &qe) {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":            "activity quota reached",
		"activities_used":  qe.Used,
		"activities_limit": qe.Limit,
	})
	return true
}

// readFile reads the multipart "file" field or writes a 400.
func (h *ImportHandlers) readFile(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, actMaxBodyBytes)
	if err := r.ParseMultipartForm(port.MaxUploadBytes); err != nil { //nolint:gosec // G120: body capped by MaxBytesReader above
		http.Error(w, "bad multipart form: "+err.Error(), http.StatusBadRequest)
		return nil, "", false
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return nil, "", false
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, port.MaxUploadBytes))
	if err != nil {
		http.Error(w, "read file: "+err.Error(), http.StatusBadRequest)
		return nil, "", false
	}
	return raw, header.Filename, true
}

func (h *ImportHandlers) upload(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	raw, filename, ok := h.readFile(w, r)
	if !ok {
		return
	}
	out, err := h.Imports.IngestUpload(r.Context(), port.UploadInput{UserID: me, Filename: filename, Raw: raw})
	if err != nil {
		if !h.writeQuotaReached(w, err) {
			writeActionError(w, err, "ingest failed: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activity_id": out.ActivityID.String(),
		"source_id":   out.SourceID.String(),
		"action":      out.Action,
		"filename":    filename,
	})
}

// actManualBody is the manual-entry JSON; type, start_time (RFC 3339) and
// elapsed_duration_s are required.
type actManualBody struct {
	Type            string  `json:"type"`
	Discipline      string  `json:"discipline"`
	Title           string  `json:"title"`
	Description     string  `json:"description"`
	StartTime       string  `json:"start_time"`
	Timezone        string  `json:"timezone"`
	ElapsedDuration float64 `json:"elapsed_duration_s"`
	MovingDuration  float64 `json:"moving_duration_s"`
	IsVirtual       bool    `json:"is_virtual"`
	IsEbike         bool    `json:"is_ebike"`
	IsCommute       bool    `json:"is_commute"`
	IsRace          bool    `json:"is_race"`
	CustomSubtype   string  `json:"custom_subtype"`
	// Optional; attached as the user's choice (blank = sport default).
	GearID string `json:"gear_id"`

	DistanceM       *float64 `json:"distance_m"`
	ElevationGainM  *float64 `json:"elevation_gain_m"`
	AvgHeartRateBpm *int32   `json:"avg_heart_rate_bpm"`
	AvgPowerW       *int32   `json:"avg_power_w"`
	CaloriesKcal    *int32   `json:"calories_kcal"`
}

func (b actManualBody) input(userID domain.UserID) port.ManualActivity {
	return port.ManualActivity{
		UserID: userID, Type: b.Type, Discipline: b.Discipline, Title: b.Title, Description: b.Description,
		StartTime: b.StartTime, Timezone: b.Timezone, ElapsedSeconds: b.ElapsedDuration, MovingSeconds: b.MovingDuration,
		IsVirtual: b.IsVirtual, IsEbike: b.IsEbike, IsCommute: b.IsCommute, IsRace: b.IsRace, CustomSubtype: b.CustomSubtype,
		DistanceM: b.DistanceM, ElevationGainM: b.ElevationGainM, AvgHeartRateBpm: b.AvgHeartRateBpm,
		AvgPowerW: b.AvgPowerW, CaloriesKcal: b.CaloriesKcal,
	}
}

func (h *ImportHandlers) manual(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	// The quota answer comes before body validation.
	if err := h.Imports.Gate(r.Context(), me); h.writeQuotaReached(w, err) {
		return
	}
	var body actManualBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	var gearID *domain.GearID
	if body.GearID != "" {
		id, err := domain.ParseUUID[domain.GearID](body.GearID)
		if err != nil {
			http.Error(w, "invalid gear_id", http.StatusBadRequest)
			return
		}
		gearID = &id
	}
	out, err := h.Imports.Manual(r.Context(), body.input(me), gearID)
	if err != nil {
		if !h.writeQuotaReached(w, err) {
			writeActionError(w, err, "ingest failed: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activity_id": out.ActivityID.String(),
		"source_id":   out.SourceID.String(),
		"action":      out.Action,
	})
}

func (h *ImportHandlers) sourceFrom(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	target, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
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
	donor, err := domain.ParseUUID[domain.ActivityID](body.DonorID)
	if err != nil {
		http.Error(w, "invalid donor_id", http.StatusBadRequest)
		return
	}
	in := pipeline.BorrowInput{UserID: me, TargetID: target, DonorID: donor}
	for _, g := range body.Groups {
		in.Groups = append(in.Groups, domain.FieldGroup(g))
	}
	srcID, err := h.Pipeline.BorrowFrom(r.Context(), in)
	if errors.Is(err, domain.ErrNotFound) {
		http.Error(w, "activity not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.Log.Warn("source-from failed", "activity_id", target, "error", err)
		writeActionError(w, err, "source-from failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source_id": srcID.String()})
}
