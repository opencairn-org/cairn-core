package domain

import (
	"errors"
	"strings"
	"time"
)

// MaintenanceOperation names an instance-wide recomputation; the values
// mirror cairn.v1.MaintenanceOperation.
type MaintenanceOperation string

const (
	MaintenanceRematchAllSegments      MaintenanceOperation = "rematch_all_segments"
	MaintenanceRecomputeBestEfforts    MaintenanceOperation = "recompute_best_efforts"
	MaintenanceRecomputeActivityMerges MaintenanceOperation = "recompute_activity_merges"
	MaintenanceRecomputeTrainingLoad   MaintenanceOperation = "recompute_training_load"
	MaintenancePurgeSoftDeleted        MaintenanceOperation = "purge_soft_deleted"
	MaintenanceGarbageCollectBlobs     MaintenanceOperation = "garbage_collect_blobs"
)

// MaintenanceOperations lists every operation in a stable order.
var MaintenanceOperations = []MaintenanceOperation{
	MaintenanceRematchAllSegments, MaintenanceRecomputeBestEfforts, MaintenanceRecomputeActivityMerges,
	MaintenanceRecomputeTrainingLoad, MaintenancePurgeSoftDeleted, MaintenanceGarbageCollectBlobs,
}

var (
	ErrUnknownMaintenanceOperation = errors.New("unknown maintenance operation")
	ErrMaintenanceConfirmMismatch  = errors.New("confirm_phrase does not match the operation")
)

// Valid reports whether op is a known operation.
func (op MaintenanceOperation) Valid() bool {
	for _, known := range MaintenanceOperations {
		if op == known {
			return true
		}
	}
	return false
}

// ConfirmPhrase is what an admin must type to trigger op: its upper-cased name.
func (op MaintenanceOperation) ConfirmPhrase() string {
	return strings.ToUpper(string(op))
}

// Confirm checks the typed phrase (surrounding whitespace ignored, case-sensitive).
func (op MaintenanceOperation) Confirm(phrase string) error {
	if !op.Valid() {
		return ErrUnknownMaintenanceOperation
	}
	if strings.TrimSpace(phrase) != op.ConfirmPhrase() {
		return ErrMaintenanceConfirmMismatch
	}
	return nil
}

// SoftDeleteRetention is how long a soft-deleted activity stays restorable
// before the purge may remove it.
const SoftDeleteRetention = 30 * 24 * time.Hour

// BlobGCMinAge keeps objects still being written (presigned uploads, claim
// checks) out of a garbage-collection pass.
const BlobGCMinAge = 24 * time.Hour

// MaintenanceStatus is the lifecycle of a triggered operation.
type MaintenanceStatus string

const (
	MaintenanceQueued    MaintenanceStatus = "queued"
	MaintenanceRunning   MaintenanceStatus = "running"
	MaintenanceSucceeded MaintenanceStatus = "succeeded"
	MaintenanceFailed    MaintenanceStatus = "failed"
	MaintenanceCancelled MaintenanceStatus = "cancelled"
)

// Done reports whether the job has reached a terminal status.
func (s MaintenanceStatus) Done() bool {
	return s == MaintenanceSucceeded || s == MaintenanceFailed || s == MaintenanceCancelled
}

// MaintenanceJob is one triggered operation and its progress.
type MaintenanceJob struct {
	ID          string
	Operation   MaintenanceOperation
	UserIDs     []UserID
	Status      MaintenanceStatus
	Estimated   int64
	Processed   int64
	Failed      int64
	Error       string
	TriggeredBy *UserID
	QueuedAt    time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
}
