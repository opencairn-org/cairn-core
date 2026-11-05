package inmem

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ProviderRegistry is a map-backed port.ProviderRegistry for tests.
type ProviderRegistry struct {
	mu   sync.Mutex
	byID map[string]domain.ProviderMeta
}

// NewProviderRegistry seeds the registry; the metas' IDs are the keys.
func NewProviderRegistry(metas ...domain.ProviderMeta) *ProviderRegistry {
	r := &ProviderRegistry{byID: map[string]domain.ProviderMeta{}}
	for _, m := range metas {
		r.byID[m.ID] = m
	}
	return r
}

func (r *ProviderRegistry) Get(_ context.Context, id string) (domain.ProviderMeta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byID[id]
	if !ok {
		return domain.ProviderMeta{}, fmt.Errorf("provider %s: %w", id, domain.ErrNotFound)
	}
	return m, nil
}

func (r *ProviderRegistry) List(context.Context) ([]domain.ProviderMeta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.ProviderMeta, 0, len(r.byID))
	for _, m := range r.byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *ProviderRegistry) Upsert(_ context.Context, meta domain.ProviderMeta, pkg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.byID[meta.ID]; ok && cur.Package != "" && cur.Package != pkg {
		return fmt.Errorf("provider %s: %w", meta.ID, domain.ErrProviderPackageChanged)
	}
	meta.Package = pkg
	r.byID[meta.ID] = meta
	return nil
}

func (r *ProviderRegistry) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; !ok {
		return fmt.Errorf("provider %s: %w", id, domain.ErrNotFound)
	}
	delete(r.byID, id)
	return nil
}
