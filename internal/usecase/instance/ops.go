package instance

import (
	"context"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

// MergeDefaults edits the instance-wide merge policy defaults.
type MergeDefaults struct {
	Settings port.UserSettingsRepo
	Audit    *audit.Recorder
}

// Get returns the defaults and the field groups a policy may override.
func (m *MergeDefaults) Get(ctx context.Context) (map[domain.ActivityType]domain.MergePolicy, []string, error) {
	if m.Settings == nil {
		return nil, nil, port.NewActionError(port.ActionErrorUnavailable, "settings are not available")
	}
	policies, err := m.Settings.GetInstanceMergeDefaults(ctx)
	if err != nil {
		return nil, nil, internalErr("load merge defaults", err)
	}
	groups := make([]string, 0, len(domain.AllFieldGroups))
	for _, g := range domain.AllFieldGroups {
		groups = append(groups, string(g))
	}
	return policies, groups, nil
}

// Set replaces the defaults; unknown field groups are refused.
func (m *MergeDefaults) Set(ctx context.Context, actor audit.Actor, policies map[domain.ActivityType]domain.MergePolicy) error {
	if m.Settings == nil {
		return port.NewActionError(port.ActionErrorUnavailable, "settings are not available")
	}
	for t, p := range policies {
		if !t.Valid() {
			return port.NewActionError(port.ActionErrorInvalid, "unknown activity type "+string(t))
		}
		for g := range p.Overrides {
			if !validFieldGroup(g) {
				return port.NewActionError(port.ActionErrorInvalid, "unknown field group "+string(g))
			}
		}
	}
	if err := m.Settings.SetInstanceMergeDefaults(ctx, policies); err != nil {
		return internalErr("save merge defaults", err)
	}
	m.Audit.High(ctx, actor, "instance_settings.merge_defaults_updated", "instance_settings", "1", map[string]string{"types": fmt.Sprint(len(policies))})
	return nil
}

func validFieldGroup(fg domain.FieldGroup) bool {
	for _, g := range domain.AllFieldGroups {
		if g == fg {
			return true
		}
	}
	return false
}

// KeyManager runs the NATS account signing-key operations; implemented in
// package main over the nats adapter (needs the secret box and the issuer cache).
type KeyManager interface {
	Bootstrap(ctx context.Context, by *domain.UserID) (key domain.SigningKey, created bool, err error)
	Rotate(ctx context.Context, by *domain.UserID) (newKey domain.SigningKey, oldPublicKey string, err error)
}

// GearTotalsRebuilder recomputes every user's gear totals.
type GearTotalsRebuilder interface {
	RebuildAllTotals(ctx context.Context) (int, error)
}

// Ops bundles the one-shot operator actions.
type Ops struct {
	// GeocodeBackfill starts one batch in the background; nil when the geocoder is off.
	GeocodeBackfill func(limit int)
	Gear            GearTotalsRebuilder
	Keys            KeyManager
	Audit           *audit.Recorder
}

// RunGeocodeBackfill starts a batch and returns the limit applied.
func (o *Ops) RunGeocodeBackfill(ctx context.Context, actor audit.Actor, limit int) (int, error) {
	if o.GeocodeBackfill == nil {
		return 0, port.NewActionError(port.ActionErrorUnavailable, "the geocoder is disabled")
	}
	if limit <= 0 {
		limit = 50
	}
	o.GeocodeBackfill(limit)
	o.Audit.Info(ctx, actor, "ops.geocode_backfill", "instance", "", map[string]string{"limit": fmt.Sprint(limit)})
	return limit, nil
}

// RebuildGearTotals recomputes gear totals and returns the user count.
func (o *Ops) RebuildGearTotals(ctx context.Context, actor audit.Actor) (int, error) {
	if o.Gear == nil {
		return 0, port.NewActionError(port.ActionErrorUnavailable, "gear is not available")
	}
	n, err := o.Gear.RebuildAllTotals(ctx)
	if err != nil {
		return 0, internalErr("rebuild gear totals", err)
	}
	o.Audit.Info(ctx, actor, "ops.gear_totals_rebuilt", "instance", "", map[string]string{"users": fmt.Sprint(n)})
	return n, nil
}

// BootstrapKey creates the NATS account signing key once.
func (o *Ops) BootstrapKey(ctx context.Context, actor audit.Actor) (domain.SigningKey, bool, error) {
	if o.Keys == nil {
		return domain.SigningKey{}, false, port.NewActionError(port.ActionErrorUnavailable, "signing keys are not available")
	}
	key, created, err := o.Keys.Bootstrap(ctx, actor.UserID)
	if err != nil {
		return domain.SigningKey{}, false, internalErr("bootstrap account key", err)
	}
	if created {
		o.Audit.High(ctx, actor, "ops.nats_account_key_created", "signing_key", key.PublicKey, nil)
	}
	return key, created, nil
}

// RotateKey replaces the active NATS account signing key.
func (o *Ops) RotateKey(ctx context.Context, actor audit.Actor) (domain.SigningKey, string, error) {
	if o.Keys == nil {
		return domain.SigningKey{}, "", port.NewActionError(port.ActionErrorUnavailable, "signing keys are not available")
	}
	key, old, err := o.Keys.Rotate(ctx, actor.UserID)
	if err != nil {
		return domain.SigningKey{}, "", internalErr("rotate account key", err)
	}
	o.Audit.High(ctx, actor, "ops.nats_account_key_rotated", "signing_key", key.PublicKey, map[string]string{"old_public_key": old})
	return key, old, nil
}
