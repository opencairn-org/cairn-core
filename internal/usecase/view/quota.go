package view

import (
	"context"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Quota resolves a user's activity allowance: the per-user override when
// set, else the instance default; zero means unlimited. Shared by
// GET /api/quota, the upload and manual-entry gates and AuthService.GetQuota.
type Quota struct {
	activities port.ActivityReader
	quotas     port.QuotaRepo // optional
	// maxActivities is the instance default.
	maxActivities int
}

// NewQuota wires the reads; quotas may be nil.
func NewQuota(activities port.ActivityReader, quotas port.QuotaRepo, maxActivities int) *Quota {
	return &Quota{activities: activities, quotas: quotas, maxActivities: maxActivities}
}

// QuotaStatus is a user's usage against the effective limit.
type QuotaStatus struct {
	Used, Limit int
	Unlimited   bool // Limit == 0
	OverLimit   bool // Used >= Limit and not unlimited
}

// Limit is the effective activity limit; 0 means unlimited.
func (q *Quota) Limit(ctx context.Context, userID domain.UserID) int {
	limit := q.maxActivities
	if q.quotas != nil {
		if override, err := q.quotas.GetMaxActivities(ctx, userID); err == nil && override != nil {
			limit = *override
		}
	}
	return max(limit, 0)
}

// Status counts the user's activities against the limit. A counting failure
// is returned, with a status that treats usage as zero so a transient
// hiccup never wrongly blocks an upload.
func (q *Quota) Status(ctx context.Context, userID domain.UserID) (QuotaStatus, error) {
	st := QuotaStatus{Limit: q.Limit(ctx, userID)}
	st.Unlimited = st.Limit == 0
	totals, err := q.activities.ActivityTotals(ctx, userID)
	if err != nil {
		return st, fmt.Errorf("activity totals: %w", err)
	}
	st.Used = totals.Count
	st.OverLimit = !st.Unlimited && st.Used >= st.Limit
	return st, nil
}
