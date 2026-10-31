package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/postgres"
	"github.com/opencairn-org/cairn-core/internal/auth"
	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/db"
	"github.com/opencairn-org/cairn-core/internal/usecase/keyrotation"
)

// newRotateKeyCmd re-encrypts every secret-at-rest under a new master key.
//
// Cairn encrypts secrets (OAuth tokens, OIDC + per-user provider client
// secrets, the NATS account signing-key seed) with AES-256-GCM via
// auth.SecretBox, where the master key comes from
// CAIRN_AUTH_MASTER_ENCRYPTION_KEY. Rotating that key means re-encrypting all
// of that ciphertext: decrypt with the OLD key, encrypt with the NEW key,
// preserving each column's AAD (additional authenticated data).
//
// The command is RESUMABLE: a ciphertext that no longer decrypts under the old
// key but DOES under the new key is treated as already-rotated and skipped, so
// re-running after a partial failure is safe. --dry-run validates that the
// current key decrypts everything (no new key, no writes).
//
//	# validate the current key can read every secret:
//	cairn rotate-key --dry-run
//	# rotate (CAIRN_AUTH_MASTER_ENCRYPTION_KEY = old key, --new-key = new):
//	cairn rotate-key --new-key="$(openssl rand -base64 32)"
//
// After rotation, set CAIRN_AUTH_MASTER_ENCRYPTION_KEY to the new key and
// restart the server + workers.
func newRotateKeyCmd() *cobra.Command {
	var newKey string
	var dryRun bool
	var skipUnreadable bool

	cmd := &cobra.Command{
		Use:   "rotate-key",
		Short: "Re-encrypt all secrets-at-rest under a new master encryption key",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRotateKey(cmd.Context(), newKey, dryRun, skipUnreadable)
		},
	}
	cmd.Flags().StringVar(&newKey, "new-key", "", "new master key (base64-encoded 32 bytes, or a passphrase)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate the current key decrypts all secrets; make no changes")
	cmd.Flags().BoolVar(&skipUnreadable, "skip-unreadable", false, "rotate readable secrets even if some can't be decrypted (those are left under the old key)")
	return cmd
}

// runRotateKey is the rotate-key command body.
func runRotateKey(ctx context.Context, newKey string, dryRun, skipUnreadable bool) error {
	if newKey == "" && !dryRun {
		return errors.New("provide --new-key, or --dry-run to validate the current key")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	oldBox, newBox, err := rotateKeyBoxes(cfg.Auth.MasterEncryptionKey, newKey)
	if err != nil {
		return err
	}

	pool, err := db.Open(ctx, db.Config{
		URL:                      cfg.Database.URL,
		MaxConns:                 cfg.Database.MaxConns,
		MinConns:                 cfg.Database.MinConns,
		StatementTimeout:         cfg.Database.StatementTimeout,
		LockTimeout:              cfg.Database.LockTimeout,
		IdleInTransactionTimeout: cfg.Database.IdleInTransactionTimeout,
		ApplicationName:          "cairn-rotate-key",
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	if dryRun {
		fmt.Println("rotate-key: DRY RUN — validating the current key decrypts every secret (no writes)")
	} else {
		fmt.Println("rotate-key: re-encrypting all secrets-at-rest under the new key")
	}
	r := &keyrotation.Rotator{
		Repo: postgres.NewSecretRotationRepo(pool), Old: oldBox, New: newBox, DryRun: dryRun,
		OnUnreadable: func(column, id string) {
			fmt.Printf("  FAIL  %s/%s: ciphertext decrypts under neither the old nor the new key\n", column, id)
		},
	}
	counts, err := r.Run(ctx)
	if err != nil {
		return err
	}
	return rotateKeyFinish(counts, dryRun, skipUnreadable)
}

// rotateKeyBoxes builds the old and new secretboxes; in dry-run (no new key) new==old.
func rotateKeyBoxes(oldKey, newKey string) (*auth.SecretBox, *auth.SecretBox, error) {
	oldBox, err := auth.NewSecretBoxFromMasterKey(oldKey)
	if err != nil {
		return nil, nil, fmt.Errorf("build old secretbox: %w", err)
	}
	newBox := oldBox // dry-run: encrypt step is never taken; new==old is fine
	if newKey != "" {
		newBox, err = auth.NewSecretBoxFromMasterKey(newKey)
		if err != nil {
			return nil, nil, fmt.Errorf("build new secretbox: %w", err)
		}
	}
	return oldBox, newBox, nil
}

// rotateKeyFinish prints the summary and decides the command's outcome.
func rotateKeyFinish(c keyrotation.Counts, dryRun, skipUnreadable bool) error {
	verb := "re-encrypted"
	if dryRun {
		verb = "would re-encrypt"
	}
	fmt.Printf("rotate-key summary: %s %d, already-new %d, empty %d, failed %d\n", verb, c.Rotated, c.Already, c.Empty, c.Failed)
	if c.Failed > 0 && !skipUnreadable {
		return fmt.Errorf("%d ciphertext(s) could not be decrypted under the old OR new key — see above; "+
			"re-enter those secrets, or pass --skip-unreadable to rotate the rest", c.Failed)
	}
	if c.Failed > 0 {
		fmt.Printf("rotate-key: WARNING — %d unreadable ciphertext(s) left untouched (still under the old key); "+
			"re-enter those secrets after rotating the master key.\n", c.Failed)
	}
	if dryRun {
		fmt.Println("rotate-key: dry run OK — the current key decrypts every secret.")
	} else {
		fmt.Println("rotate-key: done. Set CAIRN_AUTH_MASTER_ENCRYPTION_KEY to the new key and restart the server + workers.")
	}
	return nil
}
