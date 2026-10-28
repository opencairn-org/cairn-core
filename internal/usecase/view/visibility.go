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

// For computes the categories viewer may see of ownerID's activity. viewer is
// nil for anonymous; hasLink is true when a valid share token was presented.
// A block in either direction hides everything.
func (v *Visibility) For(ctx context.Context, ownerID domain.UserID, viewer *domain.UserID, activityID domain.ActivityID, hasLink bool) domain.CategorySet {
	in := domain.VisibilityInput{
		IsOwner:      viewer != nil && *viewer == ownerID,
		HasValidLink: hasLink,
	}
	if in.IsOwner {
		return domain.ResolveVisibility(in)
	}
	if viewer != nil {
		blocked, follower := v.relation(ctx, *viewer, ownerID)
		if blocked {
			return domain.CategorySet{}
		}
		in.IsFollower = follower
	}
	v.loadPolicies(ctx, &in, ownerID, activityID)
	return domain.ResolveVisibility(in)
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

// loadPolicies fills the owner and per-activity policies, ignoring load errors.
func (v *Visibility) loadPolicies(ctx context.Context, in *domain.VisibilityInput, ownerID domain.UserID, activityID domain.ActivityID) {
	if v.policies == nil {
		return
	}
	if p, err := v.policies.GetUserPolicy(ctx, ownerID); err == nil {
		in.OwnerPolicy = p
	}
	if ov, err := v.policies.GetActivityOverride(ctx, activityID); err == nil {
		in.ActivityPolicy = ov
	}
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

// Resolve answers "may viewer see act, and how". Owners see everything; a
// private or admin-hidden activity is invisible to everyone else.
func (v *Visibility) Resolve(ctx context.Context, act domain.Activity, viewer *domain.UserID, hasLink bool) (Viewer, bool) {
	if act.IsDeleted() {
		return Viewer{}, false
	}
	if viewer != nil && *viewer == act.UserID {
		return Viewer{Cats: v.For(ctx, act.UserID, viewer, act.ID, hasLink), IsOwner: true}, true
	}
	if act.Privacy == domain.PrivacyPrivate || act.HiddenByAdmin {
		return Viewer{}, false
	}
	cats := v.For(ctx, act.UserID, viewer, act.ID, hasLink)
	if len(cats) == 0 {
		return Viewer{}, false
	}
	return Viewer{Cats: cats, Zones: v.OwnerZones(ctx, act.UserID, false)}, true
}
