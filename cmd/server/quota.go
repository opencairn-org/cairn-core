package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// quotaStatus is one user's activity quota usage.
type quotaStatus struct {
	Used      int  // current activity count
	Limit     int  // effective limit (0 = unlimited)
	Unlimited bool // true when Limit == 0
	OverLimit bool // Used >= Limit (and not unlimited)
}

// effectiveActivityLimit resolves the per-user override (if any) over the
// instance default. 0 means unlimited.
func effectiveActivityLimit(ctx context.Context, app *App, userID domain.UserID) int {
	limit := app.QuotaMaxActivities
	if app.Quotas != nil {
		if override, err := app.Quotas.GetMaxActivities(ctx, userID); err == nil && override != nil {
			limit = *override
		}
	}
	if limit < 0 {
		limit = 0
	}
	return limit
}

// activityQuotaStatus computes a user's current usage against their effective
// limit. Errors counting are treated as "no usage" so a transient DB hiccup
// never wrongly blocks an upload.
func activityQuotaStatus(ctx context.Context, app *App, userID domain.UserID) quotaStatus {
	limit := effectiveActivityLimit(ctx, app, userID)
	used := 0
	if totals, err := app.Activities.ActivityTotals(ctx, userID); err == nil {
		used = totals.Count
	}
	st := quotaStatus{Used: used, Limit: limit, Unlimited: limit == 0}
	st.OverLimit = !st.Unlimited && used >= limit
	return st
}

// mountQuota wires the user-facing quota view + admin per-user override.
//
//	GET  /api/quota                       the caller's own usage + limit
//	PUT  /api/admin/users/{id}/quota       admin: set/clear per-user override
func mountQuota(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Quotas == nil {
		return
	}

	mux.HandleFunc("GET /api/quota", func(w http.ResponseWriter, r *http.Request) {
		me, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		st := activityQuotaStatus(r.Context(), app, me)
		writeJSON(w, http.StatusOK, map[string]any{
			"activities_used":  st.Used,
			"activities_limit": st.Limit,
			"unlimited":        st.Unlimited,
			"over_limit":       st.OverLimit,
		})
	})

	mux.HandleFunc("PUT /api/admin/users/{id}/quota", func(w http.ResponseWriter, r *http.Request) {
		admin, ok := resolveAdminUser(r, app)
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
			// MaxActivities: null clears the override (use instance default);
			// 0 means unlimited for this user; >0 sets a cap.
			MaxActivities *int `json:"max_activities"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if app.UserAdmin == nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := app.UserAdmin.SetQuota(r.Context(), restActor(r, admin), target, body.MaxActivities); err != nil {
			writeActionError(w, err, "save failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"max_activities": body.MaxActivities})
	})

	logger.Info("quota endpoints mounted")
}
