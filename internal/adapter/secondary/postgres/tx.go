package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// TxManager implements port.TxManager using pgx transactions.
//
// Nesting policy: a nested InTx call reuses the outer transaction. The
// inner block has no independent rollback boundary — if it returns an
// error, the error bubbles up unchanged and the outer block decides
// whether to roll back. This matches the use-case layer's typical
// pattern where one outer InTx wraps several repository operations.
//
// Savepoints could provide nested rollback granularity, but no current
// use case needs that and the added complexity isn't worth the cost.
type TxManager struct {
	pool *pgxpool.Pool
}

// NewTxManager wires a TxManager onto an existing connection pool. The
// pool's lifecycle (Open/Close) is owned by the caller; TxManager only
// uses it to begin transactions.
func NewTxManager(pool *pgxpool.Pool) *TxManager {
	return &TxManager{pool: pool}
}

// InTx runs fn inside a transaction.
//
//   - Top-level call: begins a new transaction with the pgx defaults
//     (read-committed isolation, deferrable=false). The transaction
//     commits when fn returns nil, rolls back on any error or panic.
//
//   - Nested call: reuses the outer transaction. fn sees the same ctx
//     value (the outer tx remains accessible via dbtx). Any error
//     returned bubbles up; the outer block decides what to do.
//
// Panics propagate with the transaction rolled back.
func (m *TxManager) InTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if _, nested := txFromCtx(ctx); nested {
		// Already inside a transaction — call fn with the same context.
		// The outer block manages commit/rollback.
		return fn(ctx)
	}

	tx, beginErr := m.pool.Begin(ctx)
	if beginErr != nil {
		return fmt.Errorf("begin tx: %w", beginErr)
	}

	// Best-effort rollback on panic. pgx's Rollback is a no-op after
	// successful Commit, so it's safe to call unconditionally in defer.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.Background())
			panic(p)
		}
		if err != nil {
			// Use a background context for rollback so a cancelled ctx
			// doesn't prevent the rollback from being sent.
			if rbErr := tx.Rollback(context.Background()); rbErr != nil &&
				!errors.Is(rbErr, pgx.ErrTxClosed) {
				err = fmt.Errorf("%w (rollback: %w)", err, rbErr)
			}
		}
	}()

	if err = fn(withTx(ctx, tx)); err != nil {
		return err
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return fmt.Errorf("commit tx: %w", commitErr)
	}
	return nil
}

// userLockSpace namespaces the per-user advisory locks (two-key form).
const userLockSpace = 0x63616972 // "cair"

// LockUser implements port.UserLock with a transaction-scoped advisory lock.
func (m *TxManager) LockUser(ctx context.Context, userID domain.UserID) error {
	tx, ok := txFromCtx(ctx)
	if !ok {
		return errors.New("lock user: not inside a transaction")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, int32(userLockSpace), userID.String()); err != nil {
		return fmt.Errorf("lock user: %w", err)
	}
	return nil
}
