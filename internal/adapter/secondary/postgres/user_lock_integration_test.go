//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestTxManagerLockUserSerialises(t *testing.T) {
	ctx, pool := requirePool(t)
	tm := NewTxManager(pool)
	user, other := domain.UserID(uuid.New()), domain.UserID(uuid.New())

	if err := tm.LockUser(ctx, user); err == nil {
		t.Fatal("LockUser outside a transaction must fail")
	}

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- tm.InTx(ctx, func(ctx context.Context) error {
			if err := tm.LockUser(ctx, user); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	// Another user is not blocked.
	if err := tm.InTx(ctx, func(ctx context.Context) error { return tm.LockUser(ctx, other) }); err != nil {
		t.Fatal(err)
	}
	// The same user waits for the first transaction to end.
	got := make(chan time.Time, 1)
	go func() {
		_ = tm.InTx(ctx, func(ctx context.Context) error {
			err := tm.LockUser(ctx, user)
			got <- time.Now()
			return err
		})
	}()
	select {
	case <-got:
		t.Fatal("second lock acquired while the first transaction held it")
	case <-time.After(300 * time.Millisecond):
	}
	releasedAt := time.Now()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case at := <-got:
		if at.Before(releasedAt) {
			t.Fatal("lock acquired before release")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second lock never acquired")
	}
}
