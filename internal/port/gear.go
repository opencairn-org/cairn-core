package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// GearRepo persists gear, its provider refs and the user's per-type defaults,
// and maintains the derived totals. Gear-attachment queries over activities
// live here too (repoint on merge, totals aggregate) so they stay next to
// the invariants they serve.
type GearRepo interface {
	// Get returns one gear with its external refs. ErrNotFound when absent.
	Get(ctx context.Context, id domain.GearID) (domain.Gear, error)
	// ListForUser returns the user's gear (with refs), active first, then by
	// name. Retired gear is included only when includeRetired is set.
	ListForUser(ctx context.Context, userID domain.UserID, includeRetired bool) ([]domain.Gear, error)
	// Save upserts the gear row by ID. Totals and refs are not written here.
	Save(ctx context.Context, g domain.Gear) error
	// Delete removes the gear; its activities lose the gear (FK SET NULL)
	// and go back to merge-assigned so recompute can re-derive them; refs
	// and defaults cascade. ErrNotFound when absent.
	Delete(ctx context.Context, id domain.GearID) error
	// Names returns id → name for the given gear (any owner; callers pass
	// ids taken from the caller's own activities).
	Names(ctx context.Context, ids []domain.GearID) (map[domain.GearID]string, error)

	// UpsertExternalRef creates or refreshes a provider ref (pending or
	// mapped). The ref's key is unique: an upsert for an existing key updates
	// its mapping and description.
	UpsertExternalRef(ctx context.Context, ref domain.GearExternalRef) error
	// GetExternalRef returns a provider ref with its owner. ErrNotFound when absent.
	GetExternalRef(ctx context.Context, key domain.GearRefKey) (domain.GearExternalRef, error)
	// ListPendingRefs returns the user's provider records awaiting a decision.
	ListPendingRefs(ctx context.Context, userID domain.UserID) ([]domain.GearExternalRef, error)
	// ListSiblingRefs returns the refs for the same provider gear id on the
	// user's other connections of the same provider account (one connection
	// per worker pool, docs/adr/0006).
	ListSiblingRefs(ctx context.Context, key domain.GearRefKey) ([]domain.GearExternalRef, error)
	// ResolveExternalRefs resolves many keys to their mapped gear at once;
	// absent and pending keys are missing from the result.
	ResolveExternalRefs(ctx context.Context, keys []domain.GearRefKey) (map[domain.GearRefKey]domain.GearID, error)

	// GetDefault returns the user's default gear for an activity type.
	GetDefault(ctx context.Context, userID domain.UserID, t domain.ActivityType) (domain.GearID, error)
	SetDefault(ctx context.Context, d domain.GearDefault) error
	ClearDefault(ctx context.Context, userID domain.UserID, t domain.ActivityType) error
	ListDefaults(ctx context.Context, userID domain.UserID) ([]domain.GearDefault, error)

	// MapPendingRef sets the gear of a ref only while it is still pending;
	// reports whether it did (false = already mapped or absent).
	MapPendingRef(ctx context.Context, key domain.GearRefKey, id domain.GearID) (bool, error)
	// ListActivityIDsForRef returns the user's live, merge-assigned
	// activities that have a non-detached source carrying this provider gear
	// id — the ones to re-derive after the ref is mapped onto other gear.
	ListActivityIDsForRef(ctx context.Context, userID domain.UserID, key domain.GearRefKey) ([]domain.ActivityID, error)
	// ListActivityIDsForGear returns the live activities attached to the gear.
	ListActivityIDsForGear(ctx context.Context, id domain.GearID) ([]domain.ActivityID, error)

	// Repoint moves refs, defaults and activity attachments from one gear to
	// another (same user). Used by the merge-gear use case inside a tx.
	Repoint(ctx context.Context, from, to domain.GearID) error
	// RecomputeTotalsForUser rewrites every gear row of the user from its
	// live (non-deleted) activities' post-override distance and moving time.
	RecomputeTotalsForUser(ctx context.Context, userID domain.UserID) error
	// ListUserIDsWithGear returns the owners of any gear, for the admin rebuild.
	ListUserIDsWithGear(ctx context.Context) ([]domain.UserID, error)
}
