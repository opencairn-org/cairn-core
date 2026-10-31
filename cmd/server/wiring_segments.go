package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// backfillSegment runs the segment rematch in the background and returns its job id.
func backfillSegment(app *App, logger *slog.Logger, userID domain.UserID, id domain.SegmentID, from, to time.Time) string {
	job := "rematch-segment:" + id.String()
	goSafe(logger, "segment rematch", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		sources, failed, err := app.SegmentAuthoring.RematchSegment(ctx, userID, id, from, to)
		logger.Info("segment rematch finished", "job_id", job, "sources", sources, "failed", failed, "error", err)
	})
	return job
}
