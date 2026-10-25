// Package gear holds the gear use cases: user CRUD, merging two records,
// per-type defaults, provider imports with the pending-decision flow, and the
// derived totals. Attachment to activities is decided in usecase/activity
// (recompute); this package maintains the gear side and asks for a re-derive
// when a mapping changes.
package gear

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// AccountLookup is the slice of the external-account port the gear use cases
// need: resolving a provider record's owner.
type AccountLookup interface {
	GetExternalAccount(ctx context.Context, id domain.ExternalAccountID) (domain.ExternalAccount, error)
}

// Recompute re-derives one activity (usecase/activity.RecomputeActivityFromSources).
type Recompute func(ctx context.Context, id domain.ActivityID) error

// Notifier tells the user a provider record awaits a decision.
type Notifier func(ctx context.Context, ref domain.GearExternalRef) error

// Service bundles the gear use cases.
type Service struct {
	gear      port.GearRepo
	accounts  AccountLookup
	tx        port.TxManager
	recompute Recompute // nil → mapping changes only move the ref
	notify    Notifier  // nil → pending refs are only listed
	now       func() time.Time
	newID     func() uuid.UUID
}

// NewService wires the use cases. now and newID default to time.Now and
// uuid.NewV7 when nil.
func NewService(
	gear port.GearRepo,
	accounts AccountLookup,
	tx port.TxManager,
	recompute Recompute,
	notify Notifier,
	now func() time.Time,
	newID func() uuid.UUID,
) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if newID == nil {
		newID = func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New()
			}
			return id
		}
	}
	return &Service{gear: gear, accounts: accounts, tx: tx, recompute: recompute, notify: notify, now: now, newID: newID}
}

// ErrNotOwner is returned when the gear or provider record belongs to another
// user. Callers map it to not-found so ids of other users are not probeable.
var ErrNotOwner = errors.New("gear: not owned by caller")

// Get loads one gear owned by userID.
func (s *Service) Get(ctx context.Context, userID domain.UserID, id domain.GearID) (domain.Gear, error) {
	g, err := s.gear.Get(ctx, id)
	if err != nil {
		return domain.Gear{}, err
	}
	if g.UserID != userID {
		return domain.Gear{}, ErrNotOwner
	}
	return g, nil
}

// List returns the user's gear, retired included on request.
func (s *Service) List(ctx context.Context, userID domain.UserID, includeRetired bool) ([]domain.Gear, error) {
	return s.gear.ListForUser(ctx, userID, includeRetired)
}

// Names returns id → name for gear ids the caller took from their own activities.
func (s *Service) Names(ctx context.Context, ids []domain.GearID) (map[domain.GearID]string, error) {
	return s.gear.Names(ctx, ids)
}

// DefaultFor returns the user's default gear for an activity type, if any.
func (s *Service) DefaultFor(ctx context.Context, userID domain.UserID, t domain.ActivityType) (domain.GearID, bool, error) {
	id, err := s.gear.GetDefault(ctx, userID, t)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.GearID{}, false, nil
	}
	if err != nil {
		return domain.GearID{}, false, err
	}
	return id, true, nil
}

// CreateInput is what a user supplies for new gear.
type CreateInput struct {
	Kind         domain.GearKind
	Name         string
	Brand        string
	Model        string
	Notes        string
	PurchaseDate *time.Time
}

// Create persists new user-managed gear.
func (s *Service) Create(ctx context.Context, userID domain.UserID, in CreateInput) (domain.Gear, error) {
	now := s.now()
	g := domain.Gear{
		ID:           domain.GearID(s.newID()),
		UserID:       userID,
		Kind:         in.Kind,
		Name:         strings.TrimSpace(in.Name),
		Brand:        strings.TrimSpace(in.Brand),
		Model:        strings.TrimSpace(in.Model),
		Notes:        in.Notes,
		PurchaseDate: in.PurchaseDate,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := g.Validate(); err != nil {
		return domain.Gear{}, err
	}
	if err := s.gear.Save(ctx, g); err != nil {
		return domain.Gear{}, err
	}
	return s.gear.Get(ctx, g.ID)
}

// Update loads the gear, applies the caller's edit and saves it. Retiring
// stamps RetiredAt; un-retiring clears it.
func (s *Service) Update(ctx context.Context, userID domain.UserID, id domain.GearID, apply func(*domain.Gear)) (domain.Gear, error) {
	g, err := s.Get(ctx, userID, id)
	if err != nil {
		return domain.Gear{}, err
	}
	wasRetired := g.Retired
	apply(&g)
	g.Name = strings.TrimSpace(g.Name)
	switch {
	case g.Retired && !wasRetired:
		now := s.now()
		g.RetiredAt = &now
	case !g.Retired && wasRetired:
		g.RetiredAt = nil
	}
	if err := g.Validate(); err != nil {
		return domain.Gear{}, err
	}
	if err := s.gear.Save(ctx, g); err != nil {
		return domain.Gear{}, err
	}
	return s.gear.Get(ctx, id)
}

// Delete removes the gear. Its activities are re-derived afterwards: a user
// choice of the deleted gear is gone, so the merge decides again (a source
// may resolve to other gear); refs and defaults cascade.
func (s *Service) Delete(ctx context.Context, userID domain.UserID, id domain.GearID) error {
	if _, err := s.Get(ctx, userID, id); err != nil {
		return err
	}
	affected, err := s.gear.ListActivityIDsForGear(ctx, id)
	if err != nil {
		return err
	}
	if err := s.gear.Delete(ctx, id); err != nil {
		return err
	}
	return s.rederive(ctx, userID, affected)
}

// Merge folds `from` into `to` (same user): refs, defaults and activity
// attachments move, `from` is deleted, totals of the user are rebuilt.
func (s *Service) Merge(ctx context.Context, userID domain.UserID, from, to domain.GearID) (domain.Gear, error) {
	if from == to {
		return domain.Gear{}, domain.ErrGearSelfMerge
	}
	if _, err := s.Get(ctx, userID, from); err != nil {
		return domain.Gear{}, err
	}
	if _, err := s.Get(ctx, userID, to); err != nil {
		return domain.Gear{}, err
	}
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.gear.Repoint(ctx, from, to); err != nil {
			return err
		}
		if err := s.gear.Delete(ctx, from); err != nil {
			return err
		}
		return s.gear.RecomputeTotalsForUser(ctx, userID)
	})
	if err != nil {
		return domain.Gear{}, fmt.Errorf("merge gear: %w", err)
	}
	return s.gear.Get(ctx, to)
}

// SetDefault makes the gear the user's default for an activity type.
func (s *Service) SetDefault(ctx context.Context, userID domain.UserID, t domain.ActivityType, id domain.GearID) error {
	if !t.Valid() {
		return domain.ErrActivityInvalidType
	}
	if _, err := s.Get(ctx, userID, id); err != nil {
		return err
	}
	return s.gear.SetDefault(ctx, domain.GearDefault{UserID: userID, ActivityType: t, GearID: id})
}

// ClearDefault removes the user's default for an activity type.
func (s *Service) ClearDefault(ctx context.Context, userID domain.UserID, t domain.ActivityType) error {
	return s.gear.ClearDefault(ctx, userID, t)
}

// ListDefaults returns the user's per-type defaults.
func (s *Service) ListDefaults(ctx context.Context, userID domain.UserID) ([]domain.GearDefault, error) {
	return s.gear.ListDefaults(ctx, userID)
}

// ---------------------------------------------------------------------------
// Provider records
// ---------------------------------------------------------------------------

// ImportFromProvider records a provider's gear record. The owner is the
// external account's user; claimedUser (the worker's ref) must agree, as must
// the provider. The domain gear is the golden record, so a record Cairn has
// not seen becomes a PENDING ref and the user is told to decide (create gear
// from it, map it onto existing gear, or leave it). A mapped ref only
// refreshes its description and follows a provider-side retirement; user
// edits to the gear itself are never clobbered.
func (s *Service) ImportFromProvider(ctx context.Context, claimedUser domain.UserID, key domain.GearRefKey, in domain.GearImport) (domain.GearExternalRef, error) {
	return s.importRef(ctx, claimedUser, key, in, false)
}

// NoteActivityGear records a provider gear id an activity carries as a
// pending ref when no gear record has described it yet (import_gear failed
// or never ran), so the user is still asked to decide. Known refs are left
// untouched.
func (s *Service) NoteActivityGear(ctx context.Context, userID domain.UserID, key domain.GearRefKey, name string) error {
	_, err := s.importRef(ctx, userID, key, domain.GearImport{Name: name}, true)
	return err
}

func (s *Service) importRef(ctx context.Context, claimedUser domain.UserID, key domain.GearRefKey, in domain.GearImport, onlyNew bool) (domain.GearExternalRef, error) {
	acct, err := s.accounts.GetExternalAccount(ctx, key.ExternalAccountID)
	if err != nil {
		return domain.GearExternalRef{}, fmt.Errorf("import gear %s/%s: account: %w", key.Provider, key.ExternalID, err)
	}
	if acct.UserID != claimedUser || acct.Provider != key.Provider {
		return domain.GearExternalRef{}, fmt.Errorf("import gear %s/%s: %w", key.Provider, key.ExternalID, ErrNotOwner)
	}
	var (
		out        domain.GearExternalRef
		newPending bool
	)
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		out, newPending, err = s.recordProviderGear(ctx, acct.UserID, key, in, onlyNew)
		return err
	})
	if err != nil {
		return domain.GearExternalRef{}, fmt.Errorf("import gear %s/%s: %w", key.Provider, key.ExternalID, err)
	}
	if newPending && s.notify != nil {
		if err := s.notify(ctx, out); err != nil {
			return out, fmt.Errorf("notify gear decision: %w", err)
		}
	}
	return out, nil
}

// recordProviderGear upserts the ref inside the import transaction and
// reports whether it is a new pending record.
func (s *Service) recordProviderGear(ctx context.Context, userID domain.UserID, key domain.GearRefKey, in domain.GearImport, onlyNew bool) (domain.GearExternalRef, bool, error) {
	ref, err := s.gear.GetExternalRef(ctx, key)
	newPending := errors.Is(err, domain.ErrNotFound)
	switch {
	case newPending:
		ref = domain.GearExternalRef{UserID: userID, Provider: key.Provider, ExternalAccountID: key.ExternalAccountID, ExternalID: key.ExternalID}
	case err != nil:
		return domain.GearExternalRef{}, false, err
	case onlyNew:
		return ref, false, nil
	}
	ref.Describe(in, s.now())
	if ref.GearID != nil && in.Retired {
		if err := s.retireFromProvider(ctx, *ref.GearID); err != nil {
			return domain.GearExternalRef{}, false, err
		}
	}
	if err := s.gear.UpsertExternalRef(ctx, ref); err != nil {
		return domain.GearExternalRef{}, false, err
	}
	return ref, newPending, nil
}

// retireFromProvider follows a provider-side retirement onto the mapped gear.
func (s *Service) retireFromProvider(ctx context.Context, id domain.GearID) error {
	g, err := s.gear.Get(ctx, id)
	if err != nil || g.Retired {
		return err
	}
	now := s.now()
	g.Retired, g.RetiredAt = true, &now
	return s.gear.Save(ctx, g)
}

// ListPending returns the user's provider records awaiting a decision.
func (s *Service) ListPending(ctx context.Context, userID domain.UserID) ([]domain.GearExternalRef, error) {
	return s.gear.ListPendingRefs(ctx, userID)
}

// CreateFromRef decides "this is new gear": creates gear from the provider's
// description and maps the ref onto it. The mapping is guarded inside the
// transaction, so two concurrent decisions cannot both create gear.
func (s *Service) CreateFromRef(ctx context.Context, userID domain.UserID, key domain.GearRefKey) (domain.Gear, error) {
	ref, err := s.ownedRef(ctx, userID, key)
	if err != nil {
		return domain.Gear{}, err
	}
	if !ref.Pending() {
		return domain.Gear{}, domain.ErrGearRefMapped
	}
	var id domain.GearID
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		var err error
		if id, err = s.createFromImport(ctx, userID, key, ref.Import()); err != nil {
			return err
		}
		mapped, err := s.gear.MapPendingRef(ctx, key, id)
		if err != nil {
			return err
		}
		if !mapped {
			return domain.ErrGearRefMapped
		}
		return s.seedDefaults(ctx, userID, id, ref)
	})
	if err != nil {
		return domain.Gear{}, fmt.Errorf("create gear from %s/%s: %w", key.Provider, key.ExternalID, err)
	}
	if err := s.afterMapping(ctx, userID, key); err != nil {
		return domain.Gear{}, err
	}
	return s.gear.Get(ctx, id)
}

// LinkRef decides "this is an alias": maps a provider gear record, pending
// or already mapped elsewhere, onto gear the user chose.
func (s *Service) LinkRef(ctx context.Context, userID domain.UserID, id domain.GearID, key domain.GearRefKey) (domain.Gear, error) {
	if _, err := s.Get(ctx, userID, id); err != nil {
		return domain.Gear{}, err
	}
	ref, err := s.ownedRef(ctx, userID, key)
	if err != nil {
		return domain.Gear{}, err
	}
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		ref.GearID = &id
		if err := s.gear.UpsertExternalRef(ctx, ref); err != nil {
			return err
		}
		return s.seedDefaults(ctx, userID, id, ref)
	})
	if err != nil {
		return domain.Gear{}, fmt.Errorf("link gear ref %s/%s: %w", key.Provider, key.ExternalID, err)
	}
	if err := s.afterMapping(ctx, userID, key); err != nil {
		return domain.Gear{}, err
	}
	return s.gear.Get(ctx, id)
}

// ownedRef loads a provider ref and checks it belongs to the user.
func (s *Service) ownedRef(ctx context.Context, userID domain.UserID, key domain.GearRefKey) (domain.GearExternalRef, error) {
	ref, err := s.gear.GetExternalRef(ctx, key)
	if err != nil {
		return domain.GearExternalRef{}, err
	}
	if ref.UserID != userID {
		return domain.GearExternalRef{}, ErrNotOwner
	}
	return ref, nil
}

// afterMapping runs outside the mapping transaction: the activities whose
// gear came from the record follow the new mapping, then totals refresh.
// Kept out of the transaction so a bike with thousands of rides does not hold
// row locks for the whole re-derive.
func (s *Service) afterMapping(ctx context.Context, userID domain.UserID, key domain.GearRefKey) error {
	ids, err := s.gear.ListActivityIDsForRef(ctx, userID, key)
	if err != nil {
		return err
	}
	return s.rederive(ctx, userID, ids)
}

// rederive recomputes the given activities (merge-assigned ones follow the
// refs; user-set ones keep their choice) and refreshes the user's totals.
func (s *Service) rederive(ctx context.Context, userID domain.UserID, ids []domain.ActivityID) error {
	if s.recompute != nil {
		for _, aid := range ids {
			if err := s.recompute(ctx, aid); err != nil {
				return fmt.Errorf("re-derive activity %s: %w", aid, err)
			}
		}
	}
	return s.gear.RecomputeTotalsForUser(ctx, userID)
}

func (s *Service) createFromImport(ctx context.Context, userID domain.UserID, key domain.GearRefKey, in domain.GearImport) (domain.GearID, error) {
	now := s.now()
	g := domain.Gear{
		ID:        domain.GearID(s.newID()),
		UserID:    userID,
		Kind:      in.Kind,
		Name:      strings.TrimSpace(in.Name),
		Brand:     strings.TrimSpace(in.Brand),
		Model:     strings.TrimSpace(in.Model),
		Notes:     in.Description,
		Retired:   in.Retired,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if !g.Kind.Valid() {
		g.Kind = domain.GearKindOther
	}
	if g.Name == "" {
		g.Name = key.Provider + " " + key.ExternalID
	}
	if g.Retired {
		g.RetiredAt = &now
	}
	if err := g.Validate(); err != nil {
		return domain.GearID{}, err
	}
	if err := s.gear.Save(ctx, g); err != nil {
		return domain.GearID{}, err
	}
	return g.ID, nil
}

// seedDefaults applies the provider's defaults where the user has none: the
// explicit per-type list, plus the kind's canonical type for a "primary"
// flag. A retired record seeds nothing.
func (s *Service) seedDefaults(ctx context.Context, userID domain.UserID, id domain.GearID, ref domain.GearExternalRef) error {
	if ref.Retired {
		return nil
	}
	types := append([]domain.ActivityType(nil), ref.DefaultFor...)
	if ref.Primary {
		switch ref.Kind {
		case domain.GearKindBike:
			types = append(types, domain.ActivityTypeRide)
		case domain.GearKindShoe:
			types = append(types, domain.ActivityTypeRun)
		}
	}
	for _, t := range types {
		if !t.Valid() {
			continue
		}
		_, err := s.gear.GetDefault(ctx, userID, t)
		if err == nil {
			continue
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if err := s.gear.SetDefault(ctx, domain.GearDefault{UserID: userID, ActivityType: t, GearID: id}); err != nil {
			return err
		}
	}
	return nil
}

// RecomputeTotals rebuilds the user's gear totals from live activities.
func (s *Service) RecomputeTotals(ctx context.Context, userID domain.UserID) error {
	return s.gear.RecomputeTotalsForUser(ctx, userID)
}

// RebuildAllTotals rebuilds totals for every user who owns gear (admin op).
// Returns the number of users processed.
func (s *Service) RebuildAllTotals(ctx context.Context) (int, error) {
	users, err := s.gear.ListUserIDsWithGear(ctx)
	if err != nil {
		return 0, err
	}
	for _, u := range users {
		if err := s.gear.RecomputeTotalsForUser(ctx, u); err != nil {
			return 0, fmt.Errorf("user %s: %w", u, err)
		}
	}
	return len(users), nil
}
