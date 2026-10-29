package inmem

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// SourcePreviewRepo is an in-memory port.SourcePreviewRepo for tests.
type SourcePreviewRepo struct {
	mu   sync.Mutex
	Rows map[domain.SourcePreviewID]domain.SourcePreview
	// ActivityOf resolves a source's activity for Get (the SQL repo joins).
	ActivityOf func(domain.SourceID) domain.ActivityID
}

var _ port.SourcePreviewRepo = (*SourcePreviewRepo)(nil)

func NewSourcePreviewRepo() *SourcePreviewRepo {
	return &SourcePreviewRepo{Rows: map[domain.SourcePreviewID]domain.SourcePreview{}}
}

func (r *SourcePreviewRepo) Create(_ context.Context, p domain.SourcePreview) (domain.SourcePreviewID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p.ID = domain.SourcePreviewID(uuid.New())
	p.Status = domain.SourcePreviewPending
	r.Rows[p.ID] = p
	return p.ID, nil
}

func (r *SourcePreviewRepo) Get(_ context.Context, id domain.SourcePreviewID) (domain.SourcePreview, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.Rows[id]
	if !ok {
		return domain.SourcePreview{}, fmt.Errorf("preview: %w", domain.ErrNotFound)
	}
	if r.ActivityOf != nil {
		p.ActivityID = r.ActivityOf(p.SourceID)
	}
	return p, nil
}

func (r *SourcePreviewRepo) FindPendingForSource(_ context.Context, sourceID domain.SourceID, now time.Time) (domain.SourcePreview, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.Rows {
		if p.SourceID == sourceID && p.Status == domain.SourcePreviewPending && now.Before(p.ExpiresAt) {
			return p, nil
		}
	}
	return domain.SourcePreview{}, fmt.Errorf("pending preview: %w", domain.ErrNotFound)
}

func (r *SourcePreviewRepo) MarkReady(_ context.Context, id domain.SourcePreviewID, parsed []byte, sourceDiff, mergedDiff []domain.FieldChange) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.Rows[id]
	if !ok {
		return domain.ErrNotFound
	}
	p.Status, p.Parsed, p.SourceDiff, p.MergedDiff = domain.SourcePreviewReady, parsed, sourceDiff, mergedDiff
	r.Rows[id] = p
	return nil
}

func (r *SourcePreviewRepo) Delete(_ context.Context, userID domain.UserID, id domain.SourcePreviewID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.Rows[id]
	if !ok || p.UserID != userID {
		return domain.ErrNotFound
	}
	delete(r.Rows, id)
	return nil
}

func (r *SourcePreviewRepo) DeleteExpired(_ context.Context, now time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id, p := range r.Rows {
		if !now.Before(p.ExpiresAt) {
			delete(r.Rows, id)
			n++
		}
	}
	return n, nil
}
