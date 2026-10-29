package main

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Webhook signing tokens — REST twins of ListWebhookSigningTokens and
// BulkRotateWebhookSigningTokens.
//
//	GET  /api/admin/webhook-tokens?provider=&limit=&offset=
//	POST /api/admin/webhook-tokens/rotate  { provider?, older_than_seconds?, external_account_ids? }

func (h *adminOpsHandlers) webhooksReady(w http.ResponseWriter, r *http.Request) bool {
	if !h.gate(w, r) {
		return false
	}
	if h.app.WebhookTokens == nil {
		http.Error(w, "webhook tokens not wired", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (h *adminOpsHandlers) listWebhookTokens(w http.ResponseWriter, r *http.Request) {
	if !h.webhooksReady(w, r) {
		return
	}
	rows, total, err := h.app.WebhookTokens.List(r.Context(), r.URL.Query().Get("provider"), parseIntQuery(r, "limit", 50), parseIntQuery(r, "offset", 0))
	if err != nil {
		writeActionError(w, err, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := map[string]any{"external_account_id": row.ExternalAccountID.String(), "user_id": row.UserID.String(), "provider": row.Provider, "last_rotated_at": nil}
		if row.RotatedAt != nil {
			item["last_rotated_at"] = row.RotatedAt.UTC()
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out, "total": total})
}

func (h *adminOpsHandlers) rotateWebhookTokens(w http.ResponseWriter, r *http.Request) {
	if !h.webhooksReady(w, r) {
		return
	}
	var body struct {
		Provider         string   `json:"provider"`
		OlderThanSeconds *int64   `json:"older_than_seconds"`
		AccountIDs       []string `json:"external_account_ids"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	sel := domain.WebhookRotationSelector{Provider: body.Provider}
	if body.OlderThanSeconds != nil {
		d := time.Duration(*body.OlderThanSeconds) * time.Second
		sel.OlderThan = &d
	}
	for _, raw := range body.AccountIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "bad external_account_id", http.StatusBadRequest)
			return
		}
		sel.AccountIDs = append(sel.AccountIDs, domain.ExternalAccountID(id))
	}
	admin, _ := resolveAdminUser(r, h.app)
	rotated, err := h.app.WebhookTokens.BulkRotate(r.Context(), restActor(r, admin), sel)
	if err != nil {
		writeActionError(w, err, "rotate failed")
		return
	}
	ids := make([]string, 0, len(rotated))
	for _, id := range rotated {
		ids = append(ids, id.String())
	}
	writeJSON(w, http.StatusOK, map[string]any{"rotated_count": len(rotated), "external_account_ids": ids})
}
