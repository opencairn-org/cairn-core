package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/opencairn-org/cairn-core/internal/auth"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// AccountKeys runs the operator's account signing-key operations (instance.KeyManager).
type AccountKeys struct {
	Keys    port.SigningKeyRepo
	Secrets *auth.SecretBox
	Tx      port.TxManager
	// Issuer's cache is reset after a rotation; nil when the bus is off.
	Issuer *CredentialIssuer
}

func (a AccountKeys) ready() error {
	if a.Keys == nil || a.Secrets == nil {
		return errors.New("signing-key repo / secret box not wired")
	}
	return nil
}

// Bootstrap creates the account key unless an active one exists.
func (a AccountKeys) Bootstrap(ctx context.Context, by *domain.UserID) (domain.SigningKey, bool, error) {
	if err := a.ready(); err != nil {
		return domain.SigningKey{}, false, err
	}
	if existing, err := a.Keys.GetActive(ctx, domain.SigningKeyPurposeNATSAccount); err == nil {
		return existing, false, nil
	}
	key, err := BootstrapAccountKey(ctx, a.Keys, a.Secrets, by)
	return key, err == nil, err
}

// Rotate replaces the active account key atomically and resets the issuer cache.
func (a AccountKeys) Rotate(ctx context.Context, by *domain.UserID) (domain.SigningKey, string, error) {
	if err := a.ready(); err != nil {
		return domain.SigningKey{}, "", err
	}
	var (
		newKey domain.SigningKey
		oldPub string
	)
	err := a.Tx.InTx(ctx, func(ctx context.Context) error {
		var e error
		newKey, oldPub, e = RotateAccountKey(ctx, a.Keys, a.Secrets, by)
		return e
	})
	if err != nil {
		return domain.SigningKey{}, "", err
	}
	if a.Issuer != nil {
		a.Issuer.InvalidateCache()
	}
	return newKey, oldPub, nil
}

// DeadLetterReplayer republishes a dead-lettered payload on the replay subject (jobs.Replayer).
type DeadLetterReplayer struct{ Bus *Bus }

// Replay publishes the payload under a fresh message id.
func (r DeadLetterReplayer) Replay(ctx context.Context, job domain.DeadLetteredJob) (string, error) {
	if r.Bus == nil {
		return "", errors.New("message bus not configured")
	}
	newMsgID := fmt.Sprintf("replay:%s:%d", job.ID.String(), time.Now().Unix())
	if err := r.Bus.Publish(ctx, "cairn.replay.unsorted", newMsgID, job.Payload); err != nil {
		return "", err
	}
	return newMsgID, nil
}

// Health reports the connection for the instance health probe.
func (b *Bus) Health() (bool, string) {
	if b == nil {
		return false, "message bus not configured"
	}
	nc := b.Conn()
	if nc == nil || !nc.IsConnected() {
		return false, "not connected"
	}
	return true, "connected to " + nc.ConnectedUrl()
}

// presenceHeartbeat is the worker presence record in the presence bucket.
type presenceHeartbeat struct {
	WorkerName   string                           `json:"worker_name"`
	WorkerKey    string                           `json:"worker_key"`
	InstanceID   string                           `json:"instance_id"`
	Version      string                           `json:"version"`
	Release      string                           `json:"release"`
	Provider     string                           `json:"provider"`
	Package      string                           `json:"package"`
	GoVersion    string                           `json:"go_version"`
	Commit       string                           `json:"commit"`
	BuildDate    string                           `json:"build_date"`
	Webhooks     bool                             `json:"webhooks"`
	Capabilities map[string]capability.Capability `json:"capabilities"`
	LastSeen     string                           `json:"last_seen"`
}

// WorkerHeartbeats reads every live presence record, deleting those older
// than staleAfter (crashed or replaced instances).
func (b *Bus) WorkerHeartbeats(ctx context.Context, coreRelease string, staleAfter time.Duration, logger *slog.Logger) []domain.WorkerHeartbeat {
	out := []domain.WorkerHeartbeat{}
	if b == nil {
		return out
	}
	kv, err := b.KV(KVWorkerPresence)
	if err != nil {
		return out
	}
	keys, _ := kv.Keys(ctx)
	now := time.Now()
	for _, k := range keys {
		entry, err := kv.Get(ctx, k)
		if err != nil {
			logger.Debug("admin workers: presence key vanished", "key", k, "error", err)
			continue
		}
		var hb presenceHeartbeat
		if json.Unmarshal(entry.Value, &hb) != nil {
			continue
		}
		seen, perr := time.Parse(time.RFC3339, hb.LastSeen)
		if perr == nil && now.Sub(seen) > staleAfter {
			_ = kv.Delete(ctx, k)
			continue
		}
		out = append(out, hb.toDomain(coreRelease, seen))
	}
	return out
}

func (hb presenceHeartbeat) toDomain(coreRelease string, seen time.Time) domain.WorkerHeartbeat {
	compatible, known := domain.ReleaseCompatible(coreRelease, hb.Release)
	out := domain.WorkerHeartbeat{
		WorkerName: hb.WorkerName, WorkerKey: hb.WorkerKey, InstanceID: hb.InstanceID, Version: hb.Version, Release: hb.Release,
		Compatible: compatible, Known: known, Provider: hb.Provider, Package: hb.Package, GoVersion: hb.GoVersion,
		Commit: hb.Commit, BuildDate: hb.BuildDate, Webhooks: hb.Webhooks, Capabilities: map[string]domain.LiveCapability{}, LastSeen: seen,
	}
	for k, c := range hb.Capabilities {
		out.Capabilities[k] = domain.LiveCapability{Read: c.Read, Write: c.Write, Backfill: c.Backfill, Granularity: c.Granularity}
	}
	return out
}
