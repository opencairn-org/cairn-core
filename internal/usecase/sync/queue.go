package sync

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

const (
	queueMaxInFlightPerAccount = 5
	queueProcessorTick         = 8 * time.Second

	// Items dispatched without a result for this long were Term'd on the
	// worker or lost; requeue (or fail at the cap) so they can't pin a slot.
	queueStaleAfter       = 30 * time.Minute
	queueStaleMaxAttempts = 5
	queueStaleReapEvery   = 5 * time.Minute
)

// fetchSourceJobBody mirrors the worker's fetch_source job input.
type fetchSourceJobBody struct {
	JobID        string `json:"job_id"`
	AccountID    string `json:"account_id"`
	UserID       string `json:"user_id"`
	Provider     string `json:"provider"`
	ExtID        string `json:"ext_id"`
	FetchStreams bool   `json:"fetch_streams"`
	Reason       string `json:"reason"`
}

// RunQueueProcessor drains the persisted import queue, paced and
// rate-limit-aware, until ctx ends. Items stay in_progress until the result
// router marks them done.
func (u *Imports) RunQueueProcessor(ctx context.Context) {
	log := u.log().With("component", "import_queue_processor")
	log.Info("import queue processor started", "tick", queueProcessorTick)
	ticker := time.NewTicker(queueProcessorTick)
	defer ticker.Stop()
	reap := time.NewTicker(queueStaleReapEvery)
	defer reap.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("import queue processor shutting down")
			return
		case <-reap.C:
			u.ReapStale(ctx, log)
		case <-ticker.C:
			accts, err := u.Queue.AccountsWithPending(ctx, 100)
			if err != nil {
				log.Warn("scan pending accounts failed", "error", err)
				continue
			}
			for _, id := range accts {
				u.DispatchAccount(ctx, log, id)
			}
		}
	}
}

// ReapStale requeues or fails items stuck in_progress.
func (u *Imports) ReapStale(ctx context.Context, log *slog.Logger) {
	requeued, failed, err := u.Queue.RequeueStale(ctx, queueStaleAfter, queueStaleMaxAttempts)
	if err != nil {
		log.Warn("stale queue reap failed", "error", err)
	} else if requeued > 0 || failed > 0 {
		log.Info("stale queue items reaped", "requeued", requeued, "failed", failed)
	}
}

// DispatchAccount claims what the account has capacity for and publishes
// fetch jobs, unless the account is rate-limited.
func (u *Imports) DispatchAccount(ctx context.Context, log *slog.Logger, id domain.ExternalAccountID) {
	acct, err := u.Accounts.GetExternalAccount(ctx, id)
	if err != nil {
		log.Warn("queue: load account failed", "account_id", id, "error", err)
		return
	}
	if domain.RateLimitedNow(acct.RateLimit, time.Now()) {
		return // paused until the window resets
	}
	counts, err := u.Queue.CountByStatus(ctx, id)
	if err != nil {
		log.Warn("queue: count by status failed", "account_id", id, "error", err)
		return
	}
	avail := queueMaxInFlightPerAccount - counts[domain.ImportStatusInProgress]
	if avail <= 0 {
		return
	}
	name, err := u.Router.AccountWorker(ctx, acct)
	if err != nil {
		log.Warn("queue: no worker pool for account", "account_id", id, "error", err)
		return
	}
	subject := domain.JobSubject("fetch_source", acct.Provider, name)
	items, err := u.Queue.ClaimPending(ctx, id, avail)
	if err != nil {
		log.Warn("queue: claim pending failed", "account_id", id, "error", err)
		return
	}
	if len(items) == 0 {
		return
	}
	for _, it := range items {
		body, _ := json.Marshal(fetchSourceJobBody{
			JobID: it.ID.String(), AccountID: id.String(), UserID: it.UserID.String(),
			Provider: it.Provider, ExtID: it.ExternalID, FetchStreams: true, Reason: "queue",
		})
		msgID := "queue:" + it.Provider + ":" + id.String() + ":" + it.ExternalID
		if err := u.Bus.Publish(ctx, subject, msgID, body); err != nil {
			_ = u.Queue.MarkFailed(ctx, it.ID, "dispatch: "+err.Error())
		}
	}
	log.Info("dispatched queue items", "account_id", id, "n", len(items))
}
