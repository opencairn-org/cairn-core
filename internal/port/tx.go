// Package port defines the secondary (driven) port interfaces that the
// use case layer depends on. Implementations live under
// internal/adapter/secondary/*.
//
// Convention
// ----------
//
// All repository methods take a context.Context. Transactional grouping
// is handled by the TxManager.InTx wrapper: repository calls made with
// the context yielded by InTx execute against the same transaction. The
// repository interfaces therefore have no Tx parameter on individual
// methods — this keeps the use-case code declarative.
//
//	err := tx.InTx(ctx, func(ctx context.Context) error {
//	    src, _ := repo.GetSource(ctx, srcID)
//	    return repo.SaveActivity(ctx, merged)
//	})
//
// Files
// -----
//
//	tx.go         - TxManager
//	activity.go   - ActivityRepo (Activity + ActivitySource methods)
//	stream.go     - StreamRepo
//	user.go       - UserSettingsRepo, UserRepo (subset)
//
// Repositories never return wrapped error structures — instead they wrap
// sentinel errors from internal/domain (domain.ErrNotFound, etc.) with
// fmt.Errorf so use-case code can branch with errors.Is.
package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// TxManager runs work in a database transaction. Repository methods called
// with the context yielded to fn execute against the same transaction;
// nested InTx calls reuse the outer transaction (savepoint semantics are
// adapter-specific).
//
// Returning an error from fn rolls back; returning nil commits.
type TxManager interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// UserLock serialises the writes that regroup one user's sources (ingest,
// re-cluster, detach) across goroutines and server instances. LockUser must
// be called inside InTx; the lock is held until that transaction ends.
type UserLock interface {
	LockUser(ctx context.Context, userID domain.UserID) error
}
