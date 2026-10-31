package main

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestGoSafeSurvivesPanic(t *testing.T) {
	done := make(chan struct{})
	goSafe(slog.New(slog.NewTextHandler(io.Discard, nil)), "t", func() {
		defer close(done)
		panic("boom")
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("task never ran")
	}
}

func TestGoSupervisedRestartsAfterPanic(t *testing.T) {
	defer func(d time.Duration) { supervisorBackoff = d }(supervisorBackoff)
	supervisorBackoff = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs atomic.Int32
	finished := make(chan struct{})
	goSupervised(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), "t", func(context.Context) {
		if runs.Add(1) < 3 {
			panic("boom")
		}
		close(finished) // a normal return is final
	})
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatalf("loop not restarted, runs=%d", runs.Load())
	}
	time.Sleep(20 * time.Millisecond)
	if n := runs.Load(); n != 3 {
		t.Fatalf("runs = %d, want 3", n)
	}
}
