package inmem

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// GearRepo is an in-memory port.GearRepo for tests. Activity attachment is
// modelled by Attach so the ref/gear → activity lookups and Delete's
// side effects can be exercised without Postgres.
type GearRepo struct {
	mu       sync.Mutex
	gear     map[domain.GearID]domain.Gear
	refs     map[domain.GearRefKey]domain.GearExternalRef
	defaults map[domain.UserID]map[domain.ActivityType]domain.GearID
	// activities: id → (user, gear, source gear-ref key, set-by)
	activities map[domain.ActivityID]inmemGearActivity
	// TotalsCalls counts RecomputeTotalsForUser invocations.
	TotalsCalls int
	// providerAccount: external account → provider account id, for siblings.
	providerAccount map[domain.ExternalAccountID]string
}

// SetProviderAccount records an external account's provider account id, so
// connections of the same provider account count as siblings.
func (m *GearRepo) SetProviderAccount(id domain.ExternalAccountID, providerAccountID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.providerAccount == nil {
		m.providerAccount = map[domain.ExternalAccountID]string{}
	}
	m.providerAccount[id] = providerAccountID
}

func (m *GearRepo) ListSiblingRefs(_ context.Context, key domain.GearRefKey) ([]domain.GearExternalRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pa, ok := m.providerAccount[key.ExternalAccountID]
	if !ok {
		return nil, nil
	}
	var out []domain.GearExternalRef
	for k, r := range m.refs {
		if k.Provider == key.Provider && k.ExternalID == key.ExternalID && k.ExternalAccountID != key.ExternalAccountID &&
			m.providerAccount[k.ExternalAccountID] == pa {
			out = append(out, r)
		}
	}
	return out, nil
}

type inmemGearActivity struct {
	userID domain.UserID
	gearID *domain.GearID
	ref    *domain.GearRefKey
	setBy  domain.GearSetBy
}

var _ port.GearRepo = (*GearRepo)(nil)

func NewGearRepo() *GearRepo {
	return &GearRepo{
		gear:       map[domain.GearID]domain.Gear{},
		refs:       map[domain.GearRefKey]domain.GearExternalRef{},
		defaults:   map[domain.UserID]map[domain.ActivityType]domain.GearID{},
		activities: map[domain.ActivityID]inmemGearActivity{},
	}
}

// Attach records an activity for the lookups (nil gear / nil ref allowed).
func (m *GearRepo) Attach(id domain.ActivityID, userID domain.UserID, gearID *domain.GearID, ref *domain.GearRefKey, setBy domain.GearSetBy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activities[id] = inmemGearActivity{userID: userID, gearID: gearID, ref: ref, setBy: setBy}
}

// ActivityGear returns the recorded gear and set-by of an activity.
func (m *GearRepo) ActivityGear(id domain.ActivityID) (*domain.GearID, domain.GearSetBy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.activities[id]
	return a.gearID, a.setBy
}

// Count returns the number of gear records.
func (m *GearRepo) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.gear)
}

func (m *GearRepo) Get(_ context.Context, id domain.GearID) (domain.Gear, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.gear[id]
	if !ok {
		return domain.Gear{}, fmt.Errorf("gear %s: %w", id, domain.ErrNotFound)
	}
	g.ExternalRefs = m.refsOf(id)
	return g, nil
}

func (m *GearRepo) refsOf(id domain.GearID) []domain.GearExternalRef {
	var out []domain.GearExternalRef
	for _, r := range m.refs {
		if r.GearID != nil && *r.GearID == id {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExternalID < out[j].ExternalID })
	return out
}

func (m *GearRepo) ListForUser(_ context.Context, userID domain.UserID, includeRetired bool) ([]domain.Gear, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Gear
	for _, g := range m.gear {
		if g.UserID == userID && (includeRetired || !g.Retired) {
			g.ExternalRefs = m.refsOf(g.ID)
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *GearRepo) Save(_ context.Context, g domain.Gear) error {
	if err := g.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g.ExternalRefs = nil
	m.gear[g.ID] = g
	return nil
}

func (m *GearRepo) Delete(_ context.Context, id domain.GearID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.gear[id]; !ok {
		return fmt.Errorf("gear %s: %w", id, domain.ErrNotFound)
	}
	delete(m.gear, id)
	for k, r := range m.refs {
		if r.GearID != nil && *r.GearID == id {
			delete(m.refs, k)
		}
	}
	for _, d := range m.defaults {
		for t, g := range d {
			if g == id {
				delete(d, t)
			}
		}
	}
	for aid, a := range m.activities {
		if a.gearID != nil && *a.gearID == id {
			a.gearID, a.setBy = nil, domain.GearSetByMerge
			m.activities[aid] = a
		}
	}
	return nil
}

func (m *GearRepo) Names(_ context.Context, ids []domain.GearID) (map[domain.GearID]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[domain.GearID]string{}
	for _, id := range ids {
		if g, ok := m.gear[id]; ok {
			out[id] = g.Name
		}
	}
	return out, nil
}

func (m *GearRepo) UpsertExternalRef(_ context.Context, ref domain.GearExternalRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs[ref.Key()] = ref
	return nil
}

func (m *GearRepo) GetExternalRef(_ context.Context, key domain.GearRefKey) (domain.GearExternalRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.refs[key]
	if !ok {
		return domain.GearExternalRef{}, fmt.Errorf("gear ref %s: %w", key.ExternalID, domain.ErrNotFound)
	}
	return r, nil
}

func (m *GearRepo) ListPendingRefs(_ context.Context, userID domain.UserID) ([]domain.GearExternalRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.GearExternalRef
	for _, r := range m.refs {
		if r.Pending() && r.UserID == userID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *GearRepo) ResolveExternalRefs(_ context.Context, keys []domain.GearRefKey) (map[domain.GearRefKey]domain.GearID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[domain.GearRefKey]domain.GearID{}
	for _, k := range keys {
		if r, ok := m.refs[k]; ok && r.GearID != nil {
			out[k] = *r.GearID
		}
	}
	return out, nil
}

func (m *GearRepo) MapPendingRef(_ context.Context, key domain.GearRefKey, id domain.GearID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.refs[key]
	if !ok || r.GearID != nil {
		return false, nil
	}
	r.GearID = &id
	m.refs[key] = r
	return true, nil
}

func (m *GearRepo) GetDefault(_ context.Context, userID domain.UserID, t domain.ActivityType) (domain.GearID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.defaults[userID][t]
	if !ok {
		return domain.GearID{}, fmt.Errorf("gear default %s: %w", t, domain.ErrNotFound)
	}
	return id, nil
}

func (m *GearRepo) SetDefault(_ context.Context, d domain.GearDefault) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.defaults[d.UserID] == nil {
		m.defaults[d.UserID] = map[domain.ActivityType]domain.GearID{}
	}
	m.defaults[d.UserID][d.ActivityType] = d.GearID
	return nil
}

func (m *GearRepo) ClearDefault(_ context.Context, userID domain.UserID, t domain.ActivityType) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.defaults[userID], t)
	return nil
}

func (m *GearRepo) ListDefaults(_ context.Context, userID domain.UserID) ([]domain.GearDefault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.GearDefault
	for t, id := range m.defaults[userID] {
		out = append(out, domain.GearDefault{UserID: userID, ActivityType: t, GearID: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ActivityType < out[j].ActivityType })
	return out, nil
}

func (m *GearRepo) ListActivityIDsForRef(_ context.Context, userID domain.UserID, key domain.GearRefKey) ([]domain.ActivityID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ActivityID
	for id, a := range m.activities {
		if a.userID == userID && a.ref != nil && *a.ref == key && a.setBy != domain.GearSetByUser {
			out = append(out, id)
		}
	}
	return out, nil
}

func (m *GearRepo) ListActivityIDsForGear(_ context.Context, id domain.GearID) ([]domain.ActivityID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ActivityID
	for aid, a := range m.activities {
		if a.gearID != nil && *a.gearID == id {
			out = append(out, aid)
		}
	}
	return out, nil
}

func (m *GearRepo) Repoint(_ context.Context, from, to domain.GearID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, r := range m.refs {
		if r.GearID != nil && *r.GearID == from {
			to := to
			r.GearID = &to
			m.refs[k] = r
		}
	}
	for _, d := range m.defaults {
		for t, g := range d {
			if g == from {
				d[t] = to
			}
		}
	}
	for aid, a := range m.activities {
		if a.gearID != nil && *a.gearID == from {
			to := to
			a.gearID = &to
			m.activities[aid] = a
		}
	}
	return nil
}

func (m *GearRepo) RecomputeTotalsForUser(context.Context, domain.UserID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TotalsCalls++
	return nil
}

func (m *GearRepo) ListUserIDsWithGear(context.Context) ([]domain.UserID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[domain.UserID]struct{}{}
	var out []domain.UserID
	for _, g := range m.gear {
		if _, ok := seen[g.UserID]; !ok {
			seen[g.UserID] = struct{}{}
			out = append(out, g.UserID)
		}
	}
	return out, nil
}
