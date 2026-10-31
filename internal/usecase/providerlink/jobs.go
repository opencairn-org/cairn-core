package providerlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ErrGearNotSupported: no live worker of the account's pool imports gear.
var ErrGearNotSupported = port.NewActionError(port.ActionErrorConflict, "no connected worker for this provider imports gear")

var (
	errNoBus       = errors.New("worker bus unavailable")
	errAccountGone = port.NewActionError(port.ActionErrorNotFound, "not found")
)

// Jobs publishes account-scoped jobs to the account's worker pool.
type Jobs struct {
	Accounts port.ExternalAccountRepo
	Bus      port.JobBus // nil: no bus wired, nothing is published
	Router   port.WorkerRouter
	Pools    port.PoolCapabilities
	Log      *slog.Logger
	Now      func() time.Time
}

func (j *Jobs) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now().UTC()
}

// Available reports whether jobs can be published at all.
func (j *Jobs) Available() bool { return j != nil && j.Bus != nil }

// owned loads the caller's account; errAccountGone for anyone else's.
func (j *Jobs) owned(ctx context.Context, userID domain.UserID, id domain.ExternalAccountID) (domain.ExternalAccount, error) {
	acct, err := j.Accounts.GetExternalAccount(ctx, id)
	if err != nil || acct.UserID != userID {
		return domain.ExternalAccount{}, errAccountGone
	}
	return acct, nil
}

// importGearJob is the JSON job a worker receives on import_gear
// (docs/gear-v0.4.0.md).
type importGearJob struct {
	JobID     string    `json:"job_id"`
	AccountID string    `json:"account_id"`
	UserID    string    `json:"user_id"`
	Provider  string    `json:"provider"`
	Reason    string    `json:"reason"`
	IssuedAt  time.Time `json:"issued_at"`
}

// ImportGear enqueues an import_gear job for the account. Only a pool that
// declares gear gets one: CAIRN_JOBS is a work queue and a job nobody
// consumes would sit there forever.
func (j *Jobs) ImportGear(ctx context.Context, acct domain.ExternalAccount, reason string) error {
	if !j.Available() {
		return errNoBus
	}
	name, err := j.Router.AccountWorker(ctx, acct)
	if err != nil {
		return err
	}
	if j.Pools == nil || !j.Pools.PoolDeclares(ctx, acct.Provider, name, capability.DataTypeGear) {
		return ErrGearNotSupported
	}
	now := j.now()
	body, err := json.Marshal(importGearJob{
		JobID: uuid.NewString(), AccountID: acct.ID.String(), UserID: acct.UserID.String(),
		Provider: acct.Provider, Reason: reason, IssuedAt: now,
	})
	if err != nil {
		return err
	}
	// Minute-bucketed like reconcile: a tick and a manual trigger within the
	// same minute collapse to one job.
	msgID := fmt.Sprintf("import_gear:%s:%s", acct.ID, now.Format("2006-01-02T15:04"))
	return j.Bus.Publish(ctx, domain.JobSubject("import_gear", acct.Provider, name), msgID, body)
}

// ScheduledGearSync is the reconcile-time publisher; a provider whose worker
// does not import gear is skipped.
func (j *Jobs) ScheduledGearSync(ctx context.Context, a domain.ExternalAccount) error {
	if err := j.ImportGear(ctx, a, "scheduled"); !errors.Is(err, ErrGearNotSupported) {
		return err
	}
	return nil
}

// SyncGearOnConnect publishes the first gear import for a freshly linked
// account. Best-effort: the next reconcile repeats it.
func (j *Jobs) SyncGearOnConnect(ctx context.Context, id domain.ExternalAccountID) {
	if j == nil || j.Accounts == nil {
		return
	}
	acct, err := j.Accounts.GetExternalAccount(ctx, id)
	if err != nil {
		return
	}
	if err := j.ImportGear(ctx, acct, "connect"); err != nil && !errors.Is(err, ErrGearNotSupported) && j.Log != nil {
		j.Log.Warn("gear sync on connect failed", "account_id", id, "error", err)
	}
}

// SyncGear is the user's "Sync gear" action on their account.
func (j *Jobs) SyncGear(ctx context.Context, userID domain.UserID, id domain.ExternalAccountID) error {
	acct, err := j.owned(ctx, userID, id)
	if err != nil {
		return err
	}
	return j.ImportGear(ctx, acct, "manual")
}

// HealthImportDays clamps a requested health-import window: 1..365, else 30.
func HealthImportDays(days int) int {
	if days > 0 && days <= 365 {
		return days
	}
	return 30
}

// ImportHealth queues a health-metric import (HRV, sleep, weight, steps,
// resting HR) of the last days for the caller's account.
func (j *Jobs) ImportHealth(ctx context.Context, userID domain.UserID, id domain.ExternalAccountID, days int) error {
	if !j.Available() {
		return port.NewActionError(port.ActionErrorUnavailable, errNoBus.Error())
	}
	acct, err := j.owned(ctx, userID, id)
	if err != nil {
		return err
	}
	now := j.now()
	body, _ := json.Marshal(map[string]any{
		"account_id": acct.ID.String(),
		"user_id":    userID.String(),
		"provider":   acct.Provider,
		"since_unix": now.AddDate(0, 0, -days).Unix(),
		"until_unix": now.Unix(),
	})
	pool, err := j.Router.AccountWorker(ctx, acct)
	if err != nil {
		return port.NewActionError(port.ActionErrorConflict, err.Error())
	}
	// Unique per request so re-triggering within the dedup window isn't dropped.
	msgID := fmt.Sprintf("import_metrics:%s:%s:%d", acct.Provider, acct.ID, now.Unix())
	if err := j.Bus.Publish(ctx, domain.JobSubject("import_metrics", acct.Provider, pool), msgID, body); err != nil {
		return fmt.Errorf("publish import_metrics: %w", err)
	}
	return nil
}
