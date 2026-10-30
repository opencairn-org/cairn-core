//go:build integration

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// freeAddr returns a loopback address with a port that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func runCLI(ctx context.Context, args ...string) error {
	root := newRootCmd()
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

// TestCLI_MigrateRotateServe drives the cobra commands the way an operator does:
// schema management, key rotation, then a full `serve` start and graceful shutdown.
func TestCLI_MigrateRotateServe(t *testing.T) {
	pool, dsn := itestDB(t)
	pool.Close()
	addr := freeAddr(t)
	for k, v := range map[string]string{
		"CAIRN_DATABASE_URL":                      dsn,
		"CAIRN_DATABASE_AUTO_MIGRATE":             "true",
		"CAIRN_AUTH_SESSION_SECRET":               "itest-session-secret",
		"CAIRN_AUTH_MASTER_ENCRYPTION_KEY":        "itest-master-encryption-key",
		"CAIRN_GEOCODER_ENABLED":                  "false",
		"CAIRN_FEDERATION_ENABLED":                "true",
		"CAIRN_EMAIL_ADAPTER":                     "none",
		"CAIRN_NATS_URL":                          "",
		"CAIRN_STORAGE_DRIVER":                    "none",
		"CAIRN_STORAGE_ACCESS_KEY_ID":             "",
		"CAIRN_LOG_LEVEL":                         "error",
		"CAIRN_HTTP_ADDR":                         addr,
		"CAIRN_HTTP_PUBLIC_BASE_URL":              "https://cairn.example",
		"CAIRN_INSTANCE_BOOTSTRAP_ADMIN_EMAIL":    "boot@example.com",
		"CAIRN_INSTANCE_BOOTSTRAP_ADMIN_PASSWORD": "bootstrap password",
	} {
		t.Setenv(k, v)
	}
	ctx := context.Background()

	for _, args := range [][]string{
		{"version"},
		{"config", "help"},
		{"gen-secrets"},
		{"migrate", "status"},
		{"migrate", "version"},
		{"migrate", "redo"},
		{"migrate", "down", "--force"},
		{"migrate", "up"},
		{"migrate", "up"},
		{"rotate-key", "--dry-run"},
		{"rotate-key", "--new-key", "itest-rotated-master-key"},
	} {
		if err := runCLI(ctx, args...); err != nil {
			t.Fatalf("cairn %v: %v", args, err)
		}
	}
	t.Setenv("CAIRN_AUTH_MASTER_ENCRYPTION_KEY", "itest-rotated-master-key")
	for _, args := range [][]string{
		{"migrate", "down"},
		{"migrate", "down-to", "x", "--force"},
		{"rotate-key"},
	} {
		if err := runCLI(ctx, args...); err == nil {
			t.Errorf("cairn %v: want an error", args)
		}
	}

	// serve: wait for readiness, hit a couple of endpoints, then shut down via context.
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runCLI(sctx, "serve") }()
	base := "http://" + addr
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(base + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("serve exited early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never became ready")
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, p := range []string{"/healthz", "/api/version", "/api/instance/features", "/.well-known/nodeinfo"} {
		resp, err := http.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %d", p, resp.StatusCode)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("serve returned %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not shut down")
	}
	// The https public URL forces Secure cookies even on a plain-http listener.
	if !secureCookiesAlways {
		t.Error("serve did not derive Secure cookies from the https public URL")
	}
	secureCookiesAlways = false
}
