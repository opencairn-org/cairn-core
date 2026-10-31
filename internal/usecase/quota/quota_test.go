package quota

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type totalsActivities struct {
	port.ActivityReader
	count int
	fail  bool
}

func (a *totalsActivities) ActivityTotals(context.Context, domain.UserID) (port.ActivityTotalsResult, error) {
	if a.fail {
		return port.ActivityTotalsResult{}, context.DeadlineExceeded
	}
	return port.ActivityTotalsResult{Count: a.count}, nil
}

type overrideQuotas struct {
	port.QuotaRepo
	override *int
}

func (q *overrideQuotas) GetMaxActivities(context.Context, domain.UserID) (*int, error) {
	return q.override, nil
}

func TestQuotaStatus(t *testing.T) {
	ctx := t.Context()
	me := domain.UserID(uuid.New())
	acts := &totalsActivities{count: 3}
	q := NewPolicy(acts, nil, 3)
	st, err := q.Status(ctx, me)
	if err != nil || st.Used != 3 || st.Limit != 3 || st.Unlimited || !st.OverLimit {
		t.Fatalf("default: %v %+v", err, st)
	}
	ten, zero, neg := 10, 0, -5
	if st, _ := NewPolicy(acts, &overrideQuotas{override: &ten}, 3).Status(ctx, me); st.Limit != 10 || st.OverLimit {
		t.Fatalf("override: %+v", st)
	}
	if st, _ := NewPolicy(acts, &overrideQuotas{override: &zero}, 3).Status(ctx, me); !st.Unlimited || st.OverLimit {
		t.Fatalf("unlimited: %+v", st)
	}
	if NewPolicy(acts, nil, neg).Limit(ctx, me) != 0 {
		t.Fatal("negative clamps to unlimited")
	}
	acts.fail = true
	if st, err := q.Status(ctx, me); err == nil || st.Used != 0 || st.OverLimit {
		t.Fatalf("counting failure reads as no usage: %v %+v", err, st)
	}
}
