package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/postgres"
	s3adapter "github.com/opencairn-org/cairn-core/internal/adapter/secondary/s3"
	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/db"
	attachmentuc "github.com/opencairn-org/cairn-core/internal/usecase/attachment"
)

// newAttachmentsCmd groups the operator commands for activity photos.
//
//	cairn attachments backfill-variants   derive thumb/medium/blurhash for photos that lack them
func newAttachmentsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "attachments", Short: "Operate on activity photos"}
	var batch int
	backfill := &cobra.Command{
		Use:   "backfill-variants",
		Short: "Derive the display variants (thumb, medium, blurhash) for photos stored before variants existed",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBackfillVariants(cmd.Context(), batch)
		},
	}
	backfill.Flags().IntVar(&batch, "batch", 100, "attachments per database page")
	cmd.AddCommand(backfill)
	return cmd
}

func runBackfillVariants(ctx context.Context, batch int) error {
	cfg, err := config.Load()
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
		ApplicationName:          "cairn-attachments",
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	blobs, err := s3adapter.New(cfg.Storage)
	if err != nil {
		return fmt.Errorf("object storage: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	uc := attachmentuc.NewVariants(postgres.NewAttachmentRepo(pool), blobs, logger)
	res, err := uc.Backfill(ctx, batch)
	fmt.Printf("backfill-variants: derived %d, failed %d\n", res.Done, res.Failed)
	return err
}
