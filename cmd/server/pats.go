package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// mountPATs exposes personal-access-token management (session-authed):
//
//	GET    /api/pats          → list (never returns the token)
//	POST   /api/pats          { name, expires_in_days? } → create (token shown once)
//	DELETE /api/pats/{id}     → revoke
//
// PATs authenticate API/CLI clients as their owner via
// `Authorization: Bearer cairn_pat_...` (resolved in resolveSessionUser).
func mountPATs(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.AccountTokens == nil {
		return
	}

	mux.HandleFunc("GET /api/pats", patsListHandler(app))
	mux.HandleFunc("POST /api/pats", patsCreateHandler(app))
	mux.HandleFunc("DELETE /api/pats/{id}", patsRevokeHandler(app))

	logger.Info("personal access token endpoints mounted")
}

func patsListHandler(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		list, err := app.AccountTokens.List(r.Context(), userID)
		if err != nil {
			writeActionError(w, err, "load failed")
			return
		}
		now := time.Now()
		out := make([]map[string]any, 0, len(list))
		for _, p := range list {
			status := "active"
			if p.RevokedAt != nil {
				status = "revoked"
			} else if !p.IsValidAt(now) {
				status = "expired"
			}
			out = append(out, map[string]any{
				"id": p.ID.String(), "name": p.Name, "prefix": p.TokenPrefix, "status": status,
				"created_at":   p.CreatedAt.UTC().Format(time.RFC3339),
				"expires_at":   fmtTimePtr(p.ExpiresAt),
				"last_used_at": fmtTimePtr(p.LastUsedAt),
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
	}
}

func patsCreateHandler(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		if !requireJSON(w, r) {
			return
		}
		var body struct {
			Name          string `json:"name"`
			ExpiresInDays int    `json:"expires_in_days"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var expiresAt *time.Time
		if body.ExpiresInDays > 0 {
			t := time.Now().UTC().Add(time.Duration(body.ExpiresInDays) * 24 * time.Hour)
			expiresAt = &t
		}
		pat, token, err := app.AccountTokens.Create(r.Context(), userID, body.Name, nil, expiresAt)
		if err != nil {
			writeActionError(w, err, "create failed")
			return
		}
		// token shown exactly once
		writeJSON(w, http.StatusCreated, map[string]any{"id": pat.ID.String(), "token": token})
	}
}

func patsRevokeHandler(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		if err := app.AccountTokens.Revoke(r.Context(), userID, domain.PATID(id)); err != nil {
			writeActionError(w, err, "revoke failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
