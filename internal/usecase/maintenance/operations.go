package maintenance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// eachSource runs fn over every active source; single failures are counted.
func (r *Runner) eachSource(ctx context.Context, id string, users []domain.UserID, fn SourceFn) error {
	var after domain.SourceID
	for {
		ids, err := r.Repo.ListSourceIDs(ctx, users, after, pageSize)
		if err != nil {
			return fmt.Errorf("list sources: %w", err)
		}
		for _, sid := range ids {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.progress(id, fn(ctx, sid) != nil)
		}
		if len(ids) < pageSize {
			return ctx.Err()
		}
		after = ids[len(ids)-1]
	}
}

// eachActivity runs fn over every live activity.
func (r *Runner) eachActivity(ctx context.Context, id string, users []domain.UserID, fn ActivityFn) error {
	var after domain.ActivityID
	for {
		ids, err := r.Repo.ListActivityIDs(ctx, users, after, pageSize)
		if err != nil {
			return fmt.Errorf("list activities: %w", err)
		}
		for _, aid := range ids {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.progress(id, fn(ctx, aid) != nil)
		}
		if len(ids) < pageSize {
			return ctx.Err()
		}
		after = ids[len(ids)-1]
	}
}

// eachUser recomputes the training load of every active user.
func (r *Runner) eachUser(ctx context.Context, id string, users []domain.UserID) error {
	var after domain.UserID
	for {
		ids, err := r.Repo.ListUserIDs(ctx, users, after, pageSize)
		if err != nil {
			return fmt.Errorf("list users: %w", err)
		}
		for _, uid := range ids {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.progress(id, r.TrainingLoad(ctx, uid) != nil)
		}
		if len(ids) < pageSize {
			return ctx.Err()
		}
		after = ids[len(ids)-1]
	}
}

// purgeSoftDeleted hard-deletes activities past the retention window and
// releases their blobs. The cut-off is fixed at start so the pass terminates.
func (r *Runner) purgeSoftDeleted(ctx context.Context, id string, users []domain.UserID) error {
	before := r.now().Add(-domain.SoftDeleteRetention)
	var after domain.ActivityID
	for {
		ids, err := r.Repo.ListSoftDeletedActivityIDs(ctx, users, before, after, pageSize)
		if err != nil {
			return fmt.Errorf("list soft-deleted: %w", err)
		}
		for _, aid := range ids {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			keys, err := r.Repo.HardDeleteActivity(ctx, aid)
			r.progress(id, err != nil)
			r.deleteBlobs(ctx, keys)
		}
		if len(ids) < pageSize {
			return ctx.Err()
		}
		after = ids[len(ids)-1]
	}
}

// deleteBlobs removes keys best-effort; a missing store or key is not an error.
func (r *Runner) deleteBlobs(ctx context.Context, keys []string) {
	if r.Blobs == nil {
		return
	}
	for _, key := range keys {
		if key != "" {
			_ = r.Blobs.Delete(ctx, key)
		}
	}
}

// collectBlobs deletes objects under the managed prefixes that no source or
// attachment references any more and that are older than BlobGCMinAge.
func (r *Runner) collectBlobs(ctx context.Context, id string) error {
	cutoff := r.now().Add(-domain.BlobGCMinAge)
	var batch []string
	var seen int64
	for _, prefix := range r.BlobPrefixes {
		err := r.BlobList.ListBlobKeys(ctx, prefix, func(key string, modified time.Time) error {
			seen++
			r.setEstimate(id, seen)
			if modified.After(cutoff) || !strings.HasPrefix(key, prefix) {
				return nil
			}
			batch = append(batch, key)
			if len(batch) < pageSize {
				return nil
			}
			err := r.sweepUnreferenced(ctx, id, batch)
			batch = batch[:0]
			return err
		})
		if err != nil {
			return fmt.Errorf("list %q: %w", prefix, err)
		}
	}
	return r.sweepUnreferenced(ctx, id, batch)
}

// sweepUnreferenced deletes the keys of batch that nothing references.
func (r *Runner) sweepUnreferenced(ctx context.Context, id string, batch []string) error {
	if len(batch) == 0 {
		return nil
	}
	referenced, err := r.Repo.ReferencedBlobKeys(ctx, batch)
	if err != nil {
		return fmt.Errorf("check references: %w", err)
	}
	for _, key := range batch {
		if !referenced[key] {
			r.progress(id, r.Blobs.Delete(ctx, key) != nil)
		}
	}
	return nil
}
