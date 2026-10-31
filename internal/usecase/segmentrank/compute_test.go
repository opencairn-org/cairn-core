package segmentrank

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type txKey struct{}

type fakeRanker struct {
	port.SegmentRanker
	fail  map[domain.SegmentID]bool
	calls []domain.SegmentID
	inTx  []bool
}

func (f *fakeRanker) RecomputeRanksForSegment(ctx context.Context, id domain.SegmentID) error {
	f.calls = append(f.calls, id)
	f.inTx = append(f.inTx, ctx.Value(txKey{}) != nil)
	if f.fail[id] {
		return errors.New("boom")
	}
	return nil
}

type fakeTx struct{ n int }

func (f *fakeTx) InTx(ctx context.Context, fn func(context.Context) error) error {
	f.n++
	return fn(context.WithValue(ctx, txKey{}, true))
}

func TestExecute_RunsInTx(t *testing.T) {
	r, tx := &fakeRanker{}, &fakeTx{}
	id := domain.SegmentID(uuid.New())
	if _, err := NewComputeSegmentRanks(r, tx).Execute(context.Background(), Input{SegmentID: id}); err != nil {
		t.Fatal(err)
	}
	if tx.n != 1 || len(r.calls) != 1 || r.calls[0] != id || !r.inTx[0] {
		t.Errorf("tx=%d calls=%v inTx=%v", tx.n, r.calls, r.inTx)
	}
}

func TestExecute_WrapsError(t *testing.T) {
	id := domain.SegmentID(uuid.New())
	r := &fakeRanker{fail: map[domain.SegmentID]bool{id: true}}
	if _, err := NewComputeSegmentRanks(r, &fakeTx{}).Execute(context.Background(), Input{SegmentID: id}); err == nil {
		t.Fatal("error swallowed")
	}
}

func TestExecuteMany_ContinuesPastFailure(t *testing.T) {
	a, b, c := domain.SegmentID(uuid.New()), domain.SegmentID(uuid.New()), domain.SegmentID(uuid.New())
	r, tx := &fakeRanker{fail: map[domain.SegmentID]bool{b: true}}, &fakeTx{}

	n, err := NewComputeSegmentRanks(r, tx).ExecuteMany(context.Background(), []domain.SegmentID{a, b, c})
	if n != 2 {
		t.Errorf("processed = %d, want 2", n)
	}
	if err == nil {
		t.Error("first error not returned")
	}
	if len(r.calls) != 3 || tx.n != 3 {
		t.Errorf("each segment needs its own tx: calls=%d tx=%d", len(r.calls), tx.n)
	}
}

func TestExecuteMany_Empty(t *testing.T) {
	n, err := NewComputeSegmentRanks(&fakeRanker{}, &fakeTx{}).ExecuteMany(context.Background(), nil)
	if n != 0 || err != nil {
		t.Errorf("got %d, %v", n, err)
	}
}
