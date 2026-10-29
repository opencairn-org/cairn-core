package main

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Maintenance — REST twins of TriggerMaintenance, ListMaintenanceJobs and
// CancelMaintenanceJob.
//
//	POST /api/admin/maintenance                    { operation, user_ids?, confirm_phrase }
//	GET  /api/admin/maintenance/jobs
//	POST /api/admin/maintenance/jobs/{id}/cancel

func (h *adminOpsHandlers) maintenanceReady(w http.ResponseWriter, r *http.Request) bool {
	if !h.gate(w, r) {
		return false
	}
	if h.app.Maintenance == nil {
		http.Error(w, "maintenance not wired", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (h *adminOpsHandlers) triggerMaintenance(w http.ResponseWriter, r *http.Request) {
	if !h.maintenanceReady(w, r) {
		return
	}
	var body struct {
		Operation     string   `json:"operation"`
		UserIDs       []string `json:"user_ids"`
		ConfirmPhrase string   `json:"confirm_phrase"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	users := make([]domain.UserID, 0, len(body.UserIDs))
	for _, raw := range body.UserIDs {
		uid, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "bad user_id", http.StatusBadRequest)
			return
		}
		users = append(users, domain.UserID(uid))
	}
	admin, _ := resolveAdminUser(r, h.app)
	job, err := h.app.Maintenance.Trigger(r.Context(), restActor(r, admin), domain.MaintenanceOperation(body.Operation), users, body.ConfirmPhrase)
	if err != nil {
		writeActionError(w, err, "trigger failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "estimated_record_count": job.Estimated, "job": maintenanceJobJSON(job)})
}

func (h *adminOpsHandlers) listMaintenanceJobs(w http.ResponseWriter, r *http.Request) {
	if !h.maintenanceReady(w, r) {
		return
	}
	jobs := h.app.Maintenance.List(r.Context())
	out := make([]map[string]any, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, maintenanceJobJSON(job))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (h *adminOpsHandlers) cancelMaintenanceJob(w http.ResponseWriter, r *http.Request) {
	if !h.maintenanceReady(w, r) {
		return
	}
	admin, _ := resolveAdminUser(r, h.app)
	job, err := h.app.Maintenance.Cancel(r.Context(), restActor(r, admin), r.PathValue("id"))
	if err != nil {
		writeActionError(w, err, "cancel failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": maintenanceJobJSON(job)})
}

func maintenanceJobJSON(job domain.MaintenanceJob) map[string]any {
	users := make([]string, 0, len(job.UserIDs))
	for _, uid := range job.UserIDs {
		users = append(users, uid.String())
	}
	out := map[string]any{
		"id": job.ID, "operation": string(job.Operation), "user_ids": users, "status": string(job.Status),
		"estimated_record_count": job.Estimated, "processed_count": job.Processed, "failed_count": job.Failed,
		"error_message": job.Error, "queued_at": job.QueuedAt, "started_at": job.StartedAt, "finished_at": job.FinishedAt,
		"triggered_by_user_id": nil,
	}
	if job.TriggeredBy != nil {
		out["triggered_by_user_id"] = job.TriggeredBy.String()
	}
	return out
}
