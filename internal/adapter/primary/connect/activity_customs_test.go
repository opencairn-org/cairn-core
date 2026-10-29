package connect

import (
	"context"
	"testing"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/besteffort"
)

type fakeCustomRepo struct {
	port.CustomBestEffortRepo
	rows []domain.CustomBestEffort
}

func (f *fakeCustomRepo) Create(_ context.Context, c domain.CustomBestEffort) (domain.CustomBestEffortID, error) {
	c.ID = domain.CustomBestEffortID(uuid.New())
	f.rows = append(f.rows, c)
	return c.ID, nil
}
func (f *fakeCustomRepo) ListForUser(context.Context, domain.UserID, domain.ActivityType) ([]domain.CustomBestEffort, error) {
	return f.rows, nil
}
func (f *fakeCustomRepo) Delete(_ context.Context, _ domain.UserID, id domain.CustomBestEffortID) error {
	f.rows = nil
	return nil
}

func TestCustomBestEffortRPCs(t *testing.T) {
	repo := &fakeCustomRepo{}
	var backfilled []domain.ActivityType
	s := &ActivityServer{
		Customs:  &besteffort.Customs{Repo: repo},
		Backfill: func(_ domain.UserID, at domain.ActivityType) { backfilled = append(backfilled, at) },
	}
	ctx := context.WithValue(t.Context(), ctxKeyUserID, domain.UserID(uuid.New()))

	if _, err := (&ActivityServer{}).ListCustomBestEfforts(ctx, &v1.ListCustomBestEffortsRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("unwired: %v", err)
	}
	if _, err := s.CreateCustomBestEffort(ctx, &v1.CreateCustomBestEffortRequest{Label: "x"}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("no type: %v", err)
	}
	res, err := s.CreateCustomBestEffort(ctx, &v1.CreateCustomBestEffortRequest{
		ActivityType: v1.ActivityType_ACTIVITY_TYPE_RUN, Metric: v1.BestEffortMetric_BEST_EFFORT_METRIC_PACE,
		WindowKind: v1.BestEffortWindowKind_BEST_EFFORT_WINDOW_KIND_DISTANCE, WindowValue: 7500, Label: "loop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetCustomBestEffort().GetWindowValue() != 7500 || len(backfilled) != 1 || backfilled[0] != domain.ActivityTypeRun {
		t.Fatalf("create %v backfill %v", res, backfilled)
	}
	list, err := s.ListCustomBestEfforts(ctx, &v1.ListCustomBestEffortsRequest{})
	if err != nil || len(list.GetCustomBestEfforts()) != 1 {
		t.Fatalf("list %v %v", err, list)
	}
	if _, err := s.DeleteCustomBestEffort(ctx, &v1.DeleteCustomBestEffortRequest{Id: res.GetCustomBestEffort().GetId()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteCustomBestEffort(ctx, &v1.DeleteCustomBestEffortRequest{Id: uuid.NewString()}); connectrpc.CodeOf(err) != connectrpc.CodeNotFound {
		t.Fatalf("gone: %v", err)
	}
}
