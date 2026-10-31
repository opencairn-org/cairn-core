// Package view holds the read-side use cases both primary adapters (REST and
// Connect) render from: who may see what of an activity, and the projections
// built on that answer.
package view

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Visibility resolves a viewer's CategorySet against an activity owner. It is
// the choke-point every non-owner read goes through; the repos are optional
// (nil → that signal is simply absent).
type Visibility struct {
	blocks   port.BlockRepo
	follows  port.FollowRepo
	policies port.VisibilityRepo
}

// NewVisibility wires the (optional) repos.
func NewVisibility(blocks port.BlockRepo, follows port.FollowRepo, policies port.VisibilityRepo) *Visibility {
	return &Visibility{blocks: blocks, follows: follows, policies: policies}
}

// For computes the categories viewer may see of act. viewer is nil for
// anonymous; hasLink is true when a valid share token was presented. The
// activity's privacy, hidden and deleted state are part of the decision
// (domain.ResolveVisibility); a block in either direction hides everything.
func (v *Visibility) For(ctx context.Context, act *domain.Activity, viewer *domain.UserID, hasLink bool) domain.CategorySet {
	return v.resolve(ctx, act, viewer, hasLink, v.owner(ctx, viewer, act.UserID))
}

// ownerContext is what For needs to know about one (viewer, owner) pair.
type ownerContext struct {
	blocked, follower bool
	policy            domain.VisibilityPolicy
}

func (v *Visibility) owner(ctx context.Context, viewer *domain.UserID, ownerID domain.UserID) ownerContext {
	var oc ownerContext
	if viewer != nil && *viewer == ownerID {
		return oc
	}
	if viewer != nil {
		oc.blocked, oc.follower = v.relation(ctx, *viewer, ownerID)
	}
	if v.policies != nil {
		if p, err := v.policies.GetUserPolicy(ctx, ownerID); err == nil {
			oc.policy = p
		}
	}
	return oc
}

func (v *Visibility) resolve(ctx context.Context, act *domain.Activity, viewer *domain.UserID, hasLink bool, oc ownerContext) domain.CategorySet {
	in := domain.VisibilityInputFor(act)
	in.IsOwner = viewer != nil && *viewer == act.UserID
	in.HasValidLink = hasLink
	if in.IsOwner {
		return domain.ResolveVisibility(in)
	}
	if oc.blocked {
		return domain.CategorySet{}
	}
	in.IsFollower, in.OwnerPolicy = oc.follower, oc.policy
	if v.policies != nil {
		if ov, err := v.policies.GetActivityOverride(ctx, act.ID); err == nil {
			in.ActivityPolicy = ov
		}
	}
	return domain.ResolveVisibility(in)
}

// Checker resolves many activities for one viewer, loading each owner's
// relation and policy once.
type Checker struct {
	v      *Visibility
	viewer *domain.UserID
	owners map[domain.UserID]ownerContext
}

// Checker starts a batch for viewer (nil = anonymous).
func (v *Visibility) Checker(viewer *domain.UserID) *Checker {
	return &Checker{v: v, viewer: viewer, owners: map[domain.UserID]ownerContext{}}
}

// For is Visibility.For without a share link, cached per owner.
func (c *Checker) For(ctx context.Context, act *domain.Activity) domain.CategorySet {
	oc, ok := c.owners[act.UserID]
	if !ok {
		oc = c.v.owner(ctx, c.viewer, act.UserID)
		c.owners[act.UserID] = oc
	}
	return c.v.resolve(ctx, act, c.viewer, false, oc)
}

// relation reports a block in either direction and whether viewer follows owner.
func (v *Visibility) relation(ctx context.Context, viewer, owner domain.UserID) (blocked, follower bool) {
	if v.blocks != nil {
		if b, err := v.blocks.IsBlockedEitherWay(ctx, viewer, owner); err == nil && b {
			return true, false
		}
	}
	if v.follows != nil {
		if f, err := v.follows.IsFollowing(ctx, viewer, owner); err == nil {
			follower = f
		}
	}
	return false, follower
}

// OwnerZones returns the owner's privacy zones for a NON-owner projection, nil
// for owner reads (an owner always sees their own coordinates) or when the
// zones cannot be loaded (fail-open on the load, fail-closed on a match).
func (v *Visibility) OwnerZones(ctx context.Context, ownerID domain.UserID, isOwner bool) []domain.PrivacyZone {
	if isOwner || v.policies == nil {
		return nil
	}
	zones, err := v.policies.ListPrivacyZones(ctx, ownerID)
	if err != nil {
		return nil
	}
	return zones
}

// Viewer describes how a non-owner may look at an activity: the categories and
// the zones to redact with. ok is false when the activity is not visible at all
// (private, hidden by an admin, or every category closed).
type Viewer struct {
	Cats    domain.CategorySet
	Zones   []domain.PrivacyZone
	IsOwner bool
}

// Resolve answers "may viewer see act, and how". Deleted activities are gone
// for everyone; owners see everything else; non-owners get what For grants.
func (v *Visibility) Resolve(ctx context.Context, act domain.Activity, viewer *domain.UserID, hasLink bool) (Viewer, bool) {
	if act.IsDeleted() {
		return Viewer{}, false
	}
	cats := v.For(ctx, &act, viewer, hasLink)
	if viewer != nil && *viewer == act.UserID {
		return Viewer{Cats: cats, IsOwner: true}, true
	}
	if len(cats) == 0 {
		return Viewer{}, false
	}
	return Viewer{Cats: cats, Zones: v.OwnerZones(ctx, act.UserID, false)}, true
}
