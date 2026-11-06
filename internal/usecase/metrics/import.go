package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/domain/health"
)

// ImportTarget names the store a worker metric is routed to.
type ImportTarget string

const (
	// TargetHealthSeries is health_samples, merged per day (GET /api/health).
	TargetHealthSeries ImportTarget = "health_series"
	// TargetMetrics is the metrics store (GET /api/metrics, MetricsService).
	TargetMetrics ImportTarget = "metrics"
)

// Refusal reasons of Import; stable log fields and counter labels.
const (
	RefusedBadUserID        = "bad_user_id"
	RefusedInvalidKey       = "invalid_key"
	RefusedValue            = "bad_value" // no value, or both
	RefusedBadJSON          = "bad_json"
	RefusedShapeMismatch    = "shape_mismatch"
	RefusedMissingTimestamp = "missing_timestamp"
	RefusedInvalid          = "invalid"
	RefusedUnavailable      = "store_unavailable"
	RefusedBadAccountID     = "bad_external_account_id"
	RefusedAccountMismatch  = "account_mismatch" // account of another user, provider or pool
)

// AccountLookup resolves the owner of an imported metric's account.
type AccountLookup interface {
	GetExternalAccount(ctx context.Context, id domain.ExternalAccountID) (domain.ExternalAccount, error)
}

// ImportRefusedError is a worker metric Import does not store; redelivery cannot change that.
type ImportRefusedError struct {
	Reason string
	Err    error
}

func (e *ImportRefusedError) Error() string {
	return "metric refused (" + e.Reason + "): " + e.Err.Error()
}

func (e *ImportRefusedError) Unwrap() error { return e.Err }

func refused(reason string, err error) error { return &ImportRefusedError{Reason: reason, Err: err} }

// ImportInput is one METRIC event of a provider worker.
type ImportInput struct {
	UserID              domain.UserID
	Key                 string
	Ts                  time.Time
	ValueNumeric        *float64
	ValueStruct         json.RawMessage
	Provider            string
	ExternalAccountID   *domain.ExternalAccountID
	ExternalID          string // empty: the metrics store derives one from key and ts
	SourceWorkerName    string
	SourceWorkerVersion string
}

// RouteImport routes time-series data-type keys (Steps, HRV, ...) to the health series, every other key to the metrics store.
func RouteImport(key string) ImportTarget {
	if capability.DataType(key).CategoryOf() == capability.CategoryTimeSeries {
		return TargetHealthSeries
	}
	return TargetMetrics
}

// DerivedExternalID is the dedup id of a worker metric sent without one.
func DerivedExternalID(key string, ts time.Time) string {
	return "derived:" + key + "@" + ts.UTC().Format(time.RFC3339Nano)
}

// Import upserts one worker metric into the store RouteImport names; a refusal is an *ImportRefusedError.
func (s *Store) Import(ctx context.Context, in ImportInput) error {
	if in.UserID == (domain.UserID{}) {
		return refused(RefusedBadUserID, errors.New("user id is required"))
	}
	if t := bytes.TrimSpace(in.ValueStruct); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		in.ValueStruct = nil
	} else if !json.Valid(t) {
		return refused(RefusedBadJSON, errors.New("value_json is not valid JSON"))
	}
	if err := s.checkAccount(ctx, in); err != nil {
		return err
	}
	if RouteImport(in.Key) == TargetHealthSeries {
		return s.importHealth(ctx, in)
	}
	return s.importMetric(ctx, in)
}

// checkAccount refuses a metric unless its account belongs to the metric's user and provider.
func (s *Store) checkAccount(ctx context.Context, in ImportInput) error {
	if s == nil || s.Accounts == nil {
		return refused(RefusedUnavailable, errors.New("account lookup is not available"))
	}
	if in.ExternalAccountID == nil {
		return refused(RefusedBadAccountID, errors.New("external_account_id is required"))
	}
	acct, err := s.Accounts.GetExternalAccount(ctx, *in.ExternalAccountID)
	if errors.Is(err, domain.ErrNotFound) {
		return refused(RefusedAccountMismatch, err)
	}
	if err != nil {
		return fmt.Errorf("metric account: %w", err)
	}
	if acct.UserID != in.UserID || acct.Provider != in.Provider {
		return refused(RefusedAccountMismatch, fmt.Errorf("account %s belongs to another user or provider", acct.ID))
	}
	return nil
}

func (s *Store) importHealth(ctx context.Context, in ImportInput) error {
	if s == nil || s.Health == nil {
		return refused(RefusedUnavailable, errors.New("health series are not available"))
	}
	if (in.ValueNumeric != nil) == (len(in.ValueStruct) > 0) {
		return refused(RefusedValue, domain.ErrMetricValueMissing)
	}
	if in.Ts.IsZero() {
		return refused(RefusedMissingTimestamp, domain.ErrMetricTimestamp)
	}
	return s.Health.SaveSamples(ctx, in.UserID, in.ExternalAccountID, []health.Sample{{
		UserID:      in.UserID,
		DataType:    capability.DataType(in.Key),
		Timestamp:   in.Ts.UTC(),
		Provider:    in.Provider,
		ExternalID:  in.ExternalID,
		Value:       in.ValueNumeric,
		ValueStruct: in.ValueStruct,
	}})
}

func (s *Store) importMetric(ctx context.Context, in ImportInput) error {
	if s.ready() != nil {
		return refused(RefusedUnavailable, errors.New("metrics are not available"))
	}
	extID := in.ExternalID
	if extID == "" {
		extID = DerivedExternalID(in.Key, in.Ts)
	}
	m := domain.Metric{
		ID: domain.MetricID(s.newID()), UserID: in.UserID, Key: in.Key, Timestamp: in.Ts.UTC(),
		ValueNumeric: in.ValueNumeric, ValueStruct: in.ValueStruct,
		Provider: in.Provider, ExternalAccountID: in.ExternalAccountID, ExternalID: extID,
		SourceWorkerName: in.SourceWorkerName, SourceWorkerVersion: in.SourceWorkerVersion,
	}
	if err := m.Validate(); err != nil {
		return refused(refusalReason(err), err)
	}
	return s.Repo.SaveMetrics(ctx, []domain.Metric{m})
}

// refusalReason maps a domain.Metric validation error onto its reason.
func refusalReason(err error) string {
	switch {
	case errors.Is(err, domain.ErrMetricKeyInvalid):
		return RefusedInvalidKey
	case errors.Is(err, domain.ErrMetricValueMissing):
		return RefusedValue
	case errors.Is(err, domain.ErrMetricShapeMismatch):
		return RefusedShapeMismatch
	case errors.Is(err, domain.ErrMetricTimestamp):
		return RefusedMissingTimestamp
	}
	return RefusedInvalid
}
