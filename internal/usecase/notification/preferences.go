package notification

import (
	"context"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Preferences resolves and edits the per-user channel matrix. v1 configures
// the email channel; in-app is always on.
type Preferences struct {
	Repo port.NotificationPreferenceRepo
}

// Resolved is one (type, channel) cell with its effective value.
type Resolved struct {
	Type     domain.NotificationType
	Channel  domain.NotificationChannel
	Enabled  bool
	Explicit bool // a stored row exists
}

func (p *Preferences) ready() error {
	if p == nil || p.Repo == nil {
		return port.NewActionError(port.ActionErrorUnavailable, "notification preferences are not available")
	}
	return nil
}

// Matrix returns every user-facing type for the channel, explicit rows
// first and the default where none is stored.
func (p *Preferences) Matrix(ctx context.Context, userID domain.UserID, channel domain.NotificationChannel) ([]Resolved, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	rows, err := p.Repo.ListForUser(ctx, userID)
	if err != nil {
		return nil, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("list preferences: %v", err))
	}
	explicit := map[domain.NotificationType]bool{}
	for _, r := range rows {
		if r.Channel == channel {
			explicit[r.Type] = r.Enabled
		}
	}
	out := make([]Resolved, 0, len(domain.UserFacingNotificationTypes))
	for _, t := range domain.UserFacingNotificationTypes {
		cell := Resolved{Type: t, Channel: channel}
		if enabled, ok := explicit[t]; ok {
			cell.Enabled, cell.Explicit = enabled, true
		} else {
			cell.Enabled = domain.DefaultChannelEnabled(t, channel)
		}
		out = append(out, cell)
	}
	return out, nil
}

// Set stores one cell; only the email channel is configurable in v1.
func (p *Preferences) Set(ctx context.Context, userID domain.UserID, t domain.NotificationType, channel domain.NotificationChannel, enabled bool) error {
	if err := p.ready(); err != nil {
		return err
	}
	if channel != domain.NotificationChannelEmail {
		return port.NewActionError(port.ActionErrorInvalid, "only the email channel is configurable")
	}
	if !domain.IsUserFacingNotificationType(t) {
		return port.NewActionError(port.ActionErrorInvalid, "unknown notification type")
	}
	if err := p.Repo.Upsert(ctx, domain.NotificationPreference{UserID: userID, Type: t, Channel: channel, Enabled: enabled, MinSeverity: domain.NotificationSeverityInfo}); err != nil {
		return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("save preference: %v", err))
	}
	return nil
}
