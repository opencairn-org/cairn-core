package instance

import (
	"context"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// MergePolicies are a user's own per-activity-type merge priorities, layered
// over the instance defaults.
type MergePolicies struct {
	Settings port.UserSettingsRepo
}

// ForUser returns the user's policies.
func (m *MergePolicies) ForUser(ctx context.Context, userID domain.UserID) (map[domain.ActivityType]domain.MergePolicy, error) {
	p, err := m.Settings.GetAllMergePolicies(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load merge policies: %w", err)
	}
	return p, nil
}

// WithDefaults returns the user's policies and the instance defaults below them.
func (m *MergePolicies) WithDefaults(ctx context.Context, userID domain.UserID) (user, defaults map[domain.ActivityType]domain.MergePolicy, err error) {
	if user, err = m.ForUser(ctx, userID); err != nil {
		return nil, nil, err
	}
	if defaults, err = m.Settings.GetInstanceMergeDefaults(ctx); err != nil {
		return nil, nil, fmt.Errorf("load merge defaults: %w", err)
	}
	return user, defaults, nil
}

// Set replaces the user's policies.
func (m *MergePolicies) Set(ctx context.Context, userID domain.UserID, policies map[domain.ActivityType]domain.MergePolicy) error {
	if err := m.Settings.SetMergePolicies(ctx, userID, policies); err != nil {
		return fmt.Errorf("save merge policies: %w", err)
	}
	return nil
}
