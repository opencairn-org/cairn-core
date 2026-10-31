// Package social holds a user's own social writes: following and blocking,
// profile privacy, clubs and engagement. The reads live in usecase/view.
package social

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Graph edits the caller's follow and block edges.
type Graph struct {
	Follows port.FollowRepo
	Blocks  port.BlockRepo
}

// Follow adds me→target unless it is a self-follow or either side blocks the other.
func (g *Graph) Follow(ctx context.Context, me, target domain.UserID) error {
	if me == target {
		return port.NewActionError(port.ActionErrorInvalid, "cannot follow yourself")
	}
	if g.Blocks != nil {
		if blocked, _ := g.Blocks.IsBlockedEitherWay(ctx, me, target); blocked {
			return port.NewActionError(port.ActionErrorForbidden, "cannot follow this user")
		}
	}
	return g.Follows.Follow(ctx, me, target)
}

// Unfollow removes me→target (idempotent).
func (g *Graph) Unfollow(ctx context.Context, me, target domain.UserID) error {
	return g.Follows.Unfollow(ctx, me, target)
}

// Block blocks target; the repo severs follows both ways.
func (g *Graph) Block(ctx context.Context, me, target domain.UserID) error {
	return g.Blocks.Block(ctx, me, target)
}

// Unblock lifts a block (idempotent).
func (g *Graph) Unblock(ctx context.Context, me, target domain.UserID) error {
	return g.Blocks.Unblock(ctx, me, target)
}

// Privacy edits the caller's profile visibility, default policy and privacy zones.
type Privacy struct {
	Users      port.UserRepo
	Visibility port.VisibilityRepo
}

// defaultZoneRadiusM applies when a zone is added without a radius.
const defaultZoneRadiusM = 200

// SetProfilePublic toggles whether the profile renders to anonymous viewers.
func (p *Privacy) SetProfilePublic(ctx context.Context, me domain.UserID, public bool) error {
	return p.Users.SetProfilePublic(ctx, me, public)
}

// Policy is the caller's default policy, the built-in default when unset.
func (p *Privacy) Policy(ctx context.Context, me domain.UserID) (domain.VisibilityPolicy, error) {
	pol, err := p.Visibility.GetUserPolicy(ctx, me)
	if err != nil {
		return domain.VisibilityPolicy{}, err
	}
	if pol.ByAudience == nil {
		pol = domain.DefaultVisibilityPolicy()
	}
	return pol, nil
}

// SetPolicy replaces the caller's default policy.
func (p *Privacy) SetPolicy(ctx context.Context, me domain.UserID, pol domain.VisibilityPolicy) error {
	return p.Visibility.SetUserPolicy(ctx, me, pol)
}

// Zones lists the caller's privacy zones.
func (p *Privacy) Zones(ctx context.Context, me domain.UserID) ([]domain.PrivacyZone, error) {
	return p.Visibility.ListPrivacyZones(ctx, me)
}

// AddZone stores a zone owned by the caller.
func (p *Privacy) AddZone(ctx context.Context, me domain.UserID, z domain.PrivacyZone) (domain.PrivacyZoneID, error) {
	z.UserID = me
	if z.RadiusM <= 0 {
		z.RadiusM = defaultZoneRadiusM
	}
	return p.Visibility.AddPrivacyZone(ctx, z)
}

// DeleteZone removes one of the caller's zones; domain.ErrNotFound otherwise.
func (p *Privacy) DeleteZone(ctx context.Context, me domain.UserID, id domain.PrivacyZoneID) error {
	return p.Visibility.DeletePrivacyZone(ctx, id, me)
}
