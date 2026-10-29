package besteffort

import (
	"context"
	"errors"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Customs manages a user's custom best-effort windows and keeps the computed
// efforts in step: a new window is backfilled over the user's activities of
// that type, a deleted one takes its efforts with it.
type Customs struct {
	Repo        port.CustomBestEffortRepo
	BestEfforts port.BestEffortRepo
	Activities  port.ActivityRepo
	Compute     *ComputeBestEffortsForSource
}

func actionInvalid(err error) error { return port.NewActionError(port.ActionErrorInvalid, err.Error()) }

// Create validates and stores a window. The caller runs Backfill afterwards
// (usually in the background) so existing activities gain the new efforts.
func (c *Customs) Create(ctx context.Context, in domain.CustomBestEffort) (domain.CustomBestEffort, error) {
	if err := in.Validate(); err != nil {
		return domain.CustomBestEffort{}, actionInvalid(err)
	}
	existing, err := c.Repo.ListForUser(ctx, in.UserID, "")
	if err != nil {
		return domain.CustomBestEffort{}, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("list custom best efforts: %v", err))
	}
	if len(existing) >= domain.MaxCustomBestEfforts {
		return domain.CustomBestEffort{}, port.NewActionError(port.ActionErrorQuota, domain.ErrCustomEffortLimit.Error())
	}
	id, err := c.Repo.Create(ctx, in)
	if errors.Is(err, domain.ErrCustomEffortDuplicate) {
		return domain.CustomBestEffort{}, port.NewActionError(port.ActionErrorConflict, err.Error())
	}
	if err != nil {
		return domain.CustomBestEffort{}, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("create custom best effort: %v", err))
	}
	in.ID = id
	return in, nil
}

// List returns the user's windows.
func (c *Customs) List(ctx context.Context, userID domain.UserID) ([]domain.CustomBestEffort, error) {
	list, err := c.Repo.ListForUser(ctx, userID, "")
	if err != nil {
		return nil, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("list custom best efforts: %v", err))
	}
	return list, nil
}

// Delete removes a window and the efforts computed for it (standard windows
// are never touched: a custom window equal to a standard one is a no-op there).
func (c *Customs) Delete(ctx context.Context, userID domain.UserID, id domain.CustomBestEffortID) error {
	list, err := c.Repo.ListForUser(ctx, userID, "")
	if err != nil {
		return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("list custom best efforts: %v", err))
	}
	var target *domain.CustomBestEffort
	for i := range list {
		if list[i].ID == id {
			target = &list[i]
		}
	}
	if target == nil {
		return port.NewActionError(port.ActionErrorNotFound, "custom best effort not found")
	}
	if err := c.Repo.Delete(ctx, userID, id); err != nil {
		return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("delete custom best effort: %v", err))
	}
	if !isStandard(*target) && c.BestEfforts != nil {
		if err := c.BestEfforts.DeleteForUserWindow(ctx, userID, target.ActivityType, target.Metric, target.WindowKind, target.WindowValue); err != nil {
			return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("delete efforts: %v", err))
		}
	}
	return nil
}

// Backfill recomputes best efforts for every source of the user's activities
// of the window's type; errors of single sources are counted, not fatal.
func (c *Customs) Backfill(ctx context.Context, userID domain.UserID, activityType domain.ActivityType) (sources, failed int, err error) {
	if c.Compute == nil || c.Activities == nil {
		return 0, 0, nil
	}
	const page = 200
	for offset := 0; ; offset += page {
		acts, _, err := c.Activities.ListActivitiesFiltered(ctx, userID, port.ActivityListFilter{Type: string(activityType), Limit: page, Offset: offset})
		if err != nil {
			return sources, failed, fmt.Errorf("list activities: %w", err)
		}
		for _, a := range acts {
			n, f := c.recomputeActivity(ctx, a.ID)
			sources, failed = sources+n, failed+f
		}
		if len(acts) < page {
			return sources, failed, nil
		}
	}
}

// recomputeActivity runs the compute over each source of one activity.
func (c *Customs) recomputeActivity(ctx context.Context, id domain.ActivityID) (sources, failed int) {
	srcs, err := c.Activities.ListSourcesForActivity(ctx, id)
	if err != nil {
		return 0, 1
	}
	for _, s := range srcs {
		sources++
		if _, err := c.Compute.Execute(ctx, Input{ActivitySourceID: s.ID}); err != nil {
			failed++
		}
	}
	return sources, failed
}

func isStandard(c domain.CustomBestEffort) bool {
	w := windowSpec{Metric: c.Metric, Kind: c.WindowKind, Value: c.WindowValue}
	for _, s := range StandardWindows {
		if s == w {
			return true
		}
	}
	return false
}
