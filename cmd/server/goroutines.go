package main

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"
)

// goSafe runs fn in the background; a panic is logged instead of taking the
// server (and every in-flight ingest) down.
func goSafe(log *slog.Logger, name string, fn func()) {
	go func() {
		defer recoverLog(log, name)
		fn()
	}()
}

// supervisorBackoff is the pause before a panicked background loop restarts.
var supervisorBackoff = 10 * time.Second

// goSupervised runs a long-lived loop and restarts it after a panic until ctx
// ends. A loop that returns normally is not restarted.
func goSupervised(ctx context.Context, log *slog.Logger, name string, loop func(context.Context)) {
	go func() {
		for {
			if !runRecovered(log, name, func() { loop(ctx) }) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(supervisorBackoff):
				log.Warn("restarting background loop after panic", "loop", name)
			}
		}
	}()
}

// runRecovered runs fn and reports whether it panicked.
func runRecovered(log *slog.Logger, name string, fn func()) (panicked bool) {
	defer func() {
		if p := recover(); p != nil {
			panicked = true
			log.Error("background task panicked", "task", name, "panic", p, "stack", string(debug.Stack()))
		}
	}()
	fn()
	return false
}

func recoverLog(log *slog.Logger, name string) {
	if p := recover(); p != nil {
		log.Error("background task panicked", "task", name, "panic", p, "stack", string(debug.Stack()))
	}
}
