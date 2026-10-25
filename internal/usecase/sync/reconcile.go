// Package sync holds use cases that bridge external-provider state
// with Cairn-local state via the NATS job bus. The principal use case
// is ReconcileExternalAccount — the safety net that catches missed
// webhooks and serves as the primary import path for accounts where
// webhook subscription is unavailable.
package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ReconcileExternalAccount publishes a reconcile job for one or more
// active external accounts. The job is consumed by the provider's
// worker, which:
//
//  1. Calls the provider's "list activities since <sync_watermark>" API.
//  2. For each activity ID returned, publishes a fetch_source sub-job
//     with reason="reconcile" (idempotent via Stage-1 dedup).
//  3. Reports the highest start_time observed back as the new
//     watermark (server updates external_accounts.sync_watermark on the
//     result).
//
// Reconciliation acts as two things at once:
//   - **Drift safety net** for webhook-subscribed accounts (Strava can
//     drop webhooks during their outages; daily reconcile catches
//     anything missed)
//   - **Primary import path** for accounts whose provider doesn't
//     support webhooks (every 5 min poll, configurable)
//
// The schedule is encoded in the repository's ListAccountsDueForReconcile
// query, driven by instance_settings — not in this use case.
//
// See docs/architecture.md §6.4 for the full reconciliation design
// including how the worker diff's activity-IDs against locally-known
// state and what edge cases (race with webhook, deleted-upstream) are
// handled.
type ReconcileExternalAccount struct {
	accounts port.ExternalAccountRepo
	bus      port.JobBus
	router   port.WorkerRouter

	// budgetExhausted, when non-nil, reports whether the provider's live
	// API budget is currently spent (read from the shared rate-limit KV).
	// Scheduled runs skip such accounts instead of queueing jobs that can
	// only 429 — every poll against an exhausted budget is a wasted call.
	// Force bypasses the gate.
	budgetExhausted func(ctx context.Context, provider string) bool
	// gearSync publishes an import_gear job alongside each reconcile; it
	// decides whether the provider's worker declares gear at all. May be nil.
	gearSync func(ctx context.Context, a domain.ExternalAccount) error

	now   func() time.Time
	newID func() uuid.UUID
}

// NewReconcileExternalAccount wires the use case. now defaults to
// time.Now (UTC), newID to uuid.NewV7. budgetExhausted may be nil (no
// budget gating — e.g. tests or instances without the rate-limit KV).
func NewReconcileExternalAccount(
	accounts port.ExternalAccountRepo,
	bus port.JobBus,
	router port.WorkerRouter,
	budgetExhausted func(ctx context.Context, provider string) bool,
	gearSync func(ctx context.Context, a domain.ExternalAccount) error,
	now func() time.Time,
	newID func() uuid.UUID,
) *ReconcileExternalAccount {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if newID == nil {
		newID = func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New()
			}
			return id
		}
	}
	return &ReconcileExternalAccount{
		accounts:        accounts,
		bus:             bus,
		router:          router,
		budgetExhausted: budgetExhausted,
		gearSync:        gearSync,
		now:             now,
		newID:           newID,
	}
}

// ReconcileInput selects the target accounts. Exactly one of AccountID
// or All must be set.
type ReconcileInput struct {
	// AccountID, when set, reconciles exactly one account. Used by the
	// admin endpoint and by automatic recovery after auth-failure
	// recovery.
	AccountID *domain.ExternalAccountID

	// All, when true, finds every account due for reconciliation and
	// publishes a job per account. Driven by the scheduler (see
	// cmd/server/serve.go reconciler goroutine).
	All bool

	// Force, when true, skips the eligibility check (auth_invalid /
	// disabled status, rate-limit cooldown). For manual recovery only.
	Force bool

	// BatchSize caps the number of accounts queried in All mode. Default
	// 100; the scheduler ticks frequently enough that 100 per tick keeps
	// the queue from spiking even with thousands of accounts.
	BatchSize int

	// Reason is logged with the job and propagates to the worker for
	// observability. Common values: "scheduled", "manual", "post_reauth".
	Reason string
}

// ReconcileResult summarises what the use case did.
type ReconcileResult struct {
	AccountsScheduled int
	AccountsSkipped   int
	Errors            []ReconcileError
}

// ReconcileError pairs a per-account failure with its account ID so
// the caller can report partial success without aborting the batch.
type ReconcileError struct {
	AccountID domain.ExternalAccountID
	Err       error
}

// ErrInvalidInput is returned when the caller didn't set AccountID or All.
var ErrInvalidInput = errors.New("reconcile: either AccountID or All must be set")

// Execute publishes reconcile jobs. Per-account errors are collected in
// Result.Errors rather than aborting the batch; the use case only
// returns a top-level error for catastrophic failures (e.g., repo
// query failed).
func (uc *ReconcileExternalAccount) Execute(ctx context.Context, in ReconcileInput) (ReconcileResult, error) {
	targets, skipped, err := uc.reconcileTargets(ctx, in)
	if err != nil {
		return ReconcileResult{}, err
	}
	if skipped {
		return ReconcileResult{
			AccountsSkipped: 1,
		}, nil
	}

	reason := in.Reason
	if reason == "" {
		if in.AccountID != nil {
			reason = "manual"
		} else {
			reason = "scheduled"
		}
	}

	return uc.reconcilePublishAll(ctx, targets, reason, in.Force), nil
}

// reconcileTargets resolves the accounts to reconcile; skipped is true when the
// single requested account is not eligible.
func (uc *ReconcileExternalAccount) reconcileTargets(ctx context.Context, in ReconcileInput) ([]domain.ExternalAccount, bool, error) {
	switch {
	case in.AccountID != nil:
		a, err := uc.accounts.GetExternalAccount(ctx, *in.AccountID)
		if err != nil {
			return nil, false, fmt.Errorf("get account %s: %w", *in.AccountID, err)
		}
		if !in.Force && !a.IsEligibleForSync(uc.now()) {
			return nil, true, nil
		}
		return []domain.ExternalAccount{a}, false, nil

	case in.All:
		batch := in.BatchSize
		if batch <= 0 {
			batch = 100
		}
		list, err := uc.accounts.ListAccountsDueForReconcile(ctx, uc.now(), batch)
		if err != nil {
			return nil, false, fmt.Errorf("list due accounts: %w", err)
		}
		return list, false, nil

	default:
		return nil, false, ErrInvalidInput
	}
}

// reconcilePublishAll publishes a job per target, skipping budget-exhausted providers.
func (uc *ReconcileExternalAccount) reconcilePublishAll(ctx context.Context, targets []domain.ExternalAccount, reason string, force bool) ReconcileResult {
	res := ReconcileResult{}
	// Memoise the budget check per provider — one KV read per provider per
	// batch, not per account.
	exhausted := map[string]bool{}
	for _, a := range targets {
		if !force && uc.budgetExhausted != nil {
			if _, seen := exhausted[a.Provider]; !seen {
				exhausted[a.Provider] = uc.budgetExhausted(ctx, a.Provider)
			}
			if exhausted[a.Provider] {
				res.AccountsSkipped++
				continue
			}
		}
		if err := uc.publishOne(ctx, a, reason); err != nil {
			res.Errors = append(res.Errors, ReconcileError{
				AccountID: a.ID,
				Err:       err,
			})
			continue
		}
		res.AccountsScheduled++
	}
	return res
}

// reconcileJobBody is the wire shape of the reconcile job. Encoded as
// JSON in the NATS message body. Workers decode and dispatch to their
// provider-specific reconciler.
type reconcileJobBody struct {
	JobID       string     `json:"job_id"`
	AccountID   string     `json:"account_id"`
	UserID      string     `json:"user_id"`
	Provider    string     `json:"provider"`
	Watermark   *time.Time `json:"watermark,omitempty"`
	MaxEnqueue  int        `json:"max_enqueue,omitempty"`
	KnownExtIDs []string   `json:"known_ext_ids,omitempty"`
	WebhookSeen bool       `json:"webhook_seen"`
	Reason      string     `json:"reason"`
	IssuedAt    time.Time  `json:"issued_at"`
}

// reconcileMaxEnqueuePerRun caps how many fetch_source sub-jobs one reconcile
// run may enqueue. A safety valve: combined with the worker's bounded lookback
// window, this guarantees reconcile can never flood the job stream.
const reconcileMaxEnqueuePerRun = 500

// Workers list activities from max(now - 30d lookback floor, watermark - 1h
// drift margin) and skip anything in known_ext_ids. These mirror the worker
// constants so the known-IDs window covers everything a worker may list;
// without the list, the drift margin re-fetches the newest activity (whose
// start_time IS the watermark) on every tick.
const (
	reconcileKnownIDsLookback     = 30 * 24 * time.Hour
	reconcileWatermarkDriftMargin = time.Hour
	// reconcileKnownIDsMax bounds the job payload; a truncated list only
	// costs a few redundant (idempotent) fetches.
	reconcileKnownIDsMax = 2000
)

// publishOne publishes a single reconcile job. The msgID is bucketed
// to the minute so two scheduler ticks that fire near-simultaneously
// don't both enqueue.
func (uc *ReconcileExternalAccount) publishOne(
	ctx context.Context,
	a domain.ExternalAccount,
	reason string,
) error {
	now := uc.now()
	worker, err := uc.router.AccountWorker(ctx, a)
	if err != nil {
		return fmt.Errorf("route reconcile %s: %w", a.ID, err)
	}
	subject := domain.JobSubject("reconcile", a.Provider, worker)

	since := now.Add(-reconcileKnownIDsLookback)
	if a.SyncWatermark != nil {
		if wm := a.SyncWatermark.Add(-reconcileWatermarkDriftMargin); wm.After(since) {
			since = wm
		}
	}
	known, err := uc.accounts.ListKnownExternalIDs(ctx, a.ID, since, reconcileKnownIDsMax)
	if err != nil {
		// Degrade to no dedup list (worker re-fetches, ingest is idempotent)
		// rather than blocking the account's sync.
		slog.Warn("reconcile: list known external ids failed",
			"account_id", a.ID, "error", err)
		known = nil
	}

	// Dedup key buckets to the minute so a scheduler tick + manual
	// admin trigger within the same minute collapse to one job. The
	// stream's 5-minute dedup window covers re-publishes within the
	// hot retry path.
	msgID := fmt.Sprintf("reconcile:%s:%s", a.ID, now.UTC().Format("2006-01-02T15:04"))

	body := reconcileJobBody{
		JobID:       uc.newID().String(),
		AccountID:   a.ID.String(),
		UserID:      a.UserID.String(),
		Provider:    a.Provider,
		Watermark:   a.SyncWatermark,
		MaxEnqueue:  reconcileMaxEnqueuePerRun,
		KnownExtIDs: known,
		WebhookSeen: a.WebhookSubscribed,
		Reason:      reason,
		IssuedAt:    now,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode reconcile job: %w", err)
	}

	if err := uc.bus.Publish(ctx, subject, msgID, payload); err != nil {
		return fmt.Errorf("publish reconcile job: %w", err)
	}
	// Gear lists are small and the endpoint cheap, so they ride along with
	// every reconcile (docs/gear-v0.4.0.md). Best-effort.
	if uc.gearSync != nil {
		if err := uc.gearSync(ctx, a); err != nil {
			slog.Warn("reconcile: gear sync publish failed", "account_id", a.ID, "error", err)
		}
	}
	return nil
}
