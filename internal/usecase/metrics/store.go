// Package metrics is the user-facing metric store: paged listing, latest
// values, charting series, manual entry and the training context. Both the
// REST /api/metrics routes and MetricsService call it.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

const (
	// DefaultWindow bounds reads without an explicit range.
	DefaultWindow = 365 * 24 * time.Hour
	defaultLimit  = 50
	maxLimit      = 200
	// MaxBulkRows caps one bulk upsert.
	MaxBulkRows = 1000
)

// Store runs the metric use-cases; a nil Repo answers Unavailable.
type Store struct {
	Repo    port.MetricRepo
	Athlete port.AthleteProfileRepo // optional; fills the training context
	Now     func() time.Time
	NewID   func() uuid.UUID
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Store) newID() uuid.UUID {
	if s.NewID != nil {
		return s.NewID()
	}
	if id, err := uuid.NewV7(); err == nil {
		return id
	}
	return uuid.New()
}

func (s *Store) ready() error {
	if s == nil || s.Repo == nil {
		return port.NewActionError(port.ActionErrorUnavailable, "metrics are not available on this instance")
	}
	return nil
}

func internalErr(what string, err error) error {
	return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("%s: %v", what, err))
}

// window fills a missing range with the default look-back ending now.
func (s *Store) window(start, end time.Time) (time.Time, time.Time) {
	if end.IsZero() {
		end = s.now()
	}
	if start.IsZero() {
		start = end.Add(-DefaultWindow)
	}
	return start, end
}

// List pages the user's metrics by the filter.
func (s *Store) List(ctx context.Context, userID domain.UserID, f domain.MetricListFilter) ([]domain.Metric, int, error) {
	if err := s.ready(); err != nil {
		return nil, 0, err
	}
	for _, k := range f.Keys {
		if err := domain.ValidateMetricKey(k); err != nil {
			return nil, 0, port.NewActionError(port.ActionErrorInvalid, err.Error())
		}
	}
	f.Start, f.End = s.window(f.Start, f.End)
	if f.Limit <= 0 {
		f.Limit = defaultLimit
	}
	f.Limit = min(f.Limit, maxLimit)
	f.Offset = max(f.Offset, 0)
	rows, total, err := s.Repo.ListMetrics(ctx, userID, f)
	if err != nil {
		return nil, 0, internalErr("list metrics", err)
	}
	return rows, total, nil
}

// Latest returns the newest value per key as of asOf (nil = now).
func (s *Store) Latest(ctx context.Context, userID domain.UserID, keys []string, asOf *time.Time) (map[string]domain.Metric, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	at := s.now()
	if asOf != nil {
		at = *asOf
	}
	rows, err := s.Repo.LatestMetrics(ctx, userID, keys, at)
	if err != nil {
		return nil, internalErr("latest metrics", err)
	}
	out := make(map[string]domain.Metric, len(rows))
	for _, m := range rows {
		out[m.Key] = m
	}
	return out, nil
}

// Series answers a charting query, capped to MaxPoints.
func (s *Store) Series(ctx context.Context, userID domain.UserID, q domain.MetricSeriesQuery) ([]domain.MetricSeriesPoint, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	q.Start, q.End = s.window(q.Start, q.End)
	if err := q.Validate(); err != nil {
		return nil, port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	points, err := s.Repo.QueryMetricSeries(ctx, userID, q)
	if err != nil {
		return nil, internalErr("metric series", err)
	}
	return domain.Downsample(points, q.MaxPoints), nil
}

// UpsertInput is one manual entry or edit.
type UpsertInput struct {
	ID                *domain.MetricID // set = edit that row
	Key               string
	Ts                time.Time
	PeriodSeconds     int
	ValueNumeric      *float64
	ValueStruct       json.RawMessage
	Provider          string // defaults to manual
	Tags              []string
	Notes             string
	DedupByNaturalKey bool // reuse the row of (key, ts, provider) when one exists
}

// Upsert creates or edits one entry; created reports which happened.
func (s *Store) Upsert(ctx context.Context, userID domain.UserID, in UpsertInput) (domain.Metric, bool, error) {
	if err := s.ready(); err != nil {
		return domain.Metric{}, false, err
	}
	if in.Provider == "" {
		in.Provider = domain.MetricProviderManual
	}
	m := domain.Metric{UserID: userID, Key: in.Key, Timestamp: in.Ts.UTC(), PeriodSeconds: in.PeriodSeconds, ValueNumeric: in.ValueNumeric,
		ValueStruct: in.ValueStruct, Provider: in.Provider, Tags: in.Tags, Notes: in.Notes}
	if err := m.Validate(); err != nil {
		return domain.Metric{}, false, port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	if in.ID != nil {
		return s.edit(ctx, userID, *in.ID, m)
	}
	if in.DedupByNaturalKey {
		existing, err := s.Repo.FindMetricByNaturalKey(ctx, userID, m.Key, m.Timestamp, m.Provider)
		if err == nil {
			return s.edit(ctx, userID, existing.ID, m)
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return domain.Metric{}, false, internalErr("find metric", err)
		}
	}
	m.ID = domain.MetricID(s.newID())
	if err := s.Repo.SaveMetrics(ctx, []domain.Metric{m}); err != nil {
		return domain.Metric{}, false, internalErr("save metric", err)
	}
	saved, err := s.Repo.GetMetric(ctx, userID, m.ID)
	if err != nil {
		return m, true, nil
	}
	return saved, true, nil
}

// edit rewrites the mutable columns of an existing row; key and time stay.
func (s *Store) edit(ctx context.Context, userID domain.UserID, id domain.MetricID, m domain.Metric) (domain.Metric, bool, error) {
	existing, err := s.Repo.GetMetric(ctx, userID, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Metric{}, false, port.NewActionError(port.ActionErrorNotFound, "metric not found")
	}
	if err != nil {
		return domain.Metric{}, false, internalErr("get metric", err)
	}
	if existing.Key != m.Key {
		return domain.Metric{}, false, port.NewActionError(port.ActionErrorInvalid, "key cannot change on edit")
	}
	existing.PeriodSeconds, existing.ValueNumeric, existing.ValueStruct, existing.Tags, existing.Notes =
		m.PeriodSeconds, m.ValueNumeric, m.ValueStruct, m.Tags, m.Notes
	if err := s.Repo.UpdateMetric(ctx, existing); err != nil {
		return domain.Metric{}, false, internalErr("update metric", err)
	}
	updated, err := s.Repo.GetMetric(ctx, userID, id)
	if err != nil {
		return existing, false, nil
	}
	return updated, false, nil
}

// Delete removes one entry of the user.
func (s *Store) Delete(ctx context.Context, userID domain.UserID, id domain.MetricID) error {
	if err := s.ready(); err != nil {
		return err
	}
	err := s.Repo.DeleteMetric(ctx, userID, id)
	if errors.Is(err, domain.ErrNotFound) {
		return port.NewActionError(port.ActionErrorNotFound, "metric not found")
	}
	if err != nil {
		return internalErr("delete metric", err)
	}
	return nil
}

// BulkError reports one rejected row.
type BulkError struct {
	Row     int
	Code    string
	Message string
}

// BulkResult tallies a bulk upsert.
type BulkResult struct {
	Created, Updated, Skipped int
	Errors                    []BulkError
}

// BulkUpsert applies the rows in order; without continueOnError the first
// failure stops the run and the rows after it are counted as skipped.
func (s *Store) BulkUpsert(ctx context.Context, userID domain.UserID, rows []UpsertInput, continueOnError bool) (BulkResult, error) {
	if err := s.ready(); err != nil {
		return BulkResult{}, err
	}
	if len(rows) > MaxBulkRows {
		return BulkResult{}, port.NewActionError(port.ActionErrorInvalid, fmt.Sprintf("at most %d rows per call", MaxBulkRows))
	}
	var res BulkResult
	for i, in := range rows {
		_, created, err := s.Upsert(ctx, userID, in)
		if err == nil {
			if created {
				res.Created++
			} else {
				res.Updated++
			}
			continue
		}
		var ae *port.ActionError
		if !errors.As(err, &ae) || ae.Kind == port.ActionErrorInternal || ae.Kind == port.ActionErrorUnavailable {
			return res, err
		}
		res.Errors = append(res.Errors, BulkError{Row: i, Code: ae.Kind.String(), Message: ae.Msg})
		if !continueOnError {
			res.Skipped = len(rows) - i - 1
			break
		}
	}
	return res, nil
}

// athleteContextKeys maps the athlete profile onto the reserved metric keys.
var athleteContextKeys = map[domain.AthleteMetricKey][]string{
	domain.AthleteFTPWatts:      {"ftp_cycling_w"},
	domain.AthleteThresholdHR:   {"lthr_running_bpm", "lthr_cycling_bpm"},
	domain.AthleteMaxHR:         {"max_hr_bpm"},
	domain.AthleteRestingHR:     {"resting_hr_bpm"},
	domain.AthleteWeightKg:      {"weight_kg"},
	domain.AthleteThresholdPace: {"threshold_pace_running_s_per_km"},
}

// ContextKeys are the reserved keys the training context always reports.
var ContextKeys = []string{"weight_kg", "resting_hr_bpm", "max_hr_bpm", "hrv_rmssd_ms", "vo2max_ml_kg_min",
	"ftp_cycling_w", "lthr_running_bpm", "lthr_cycling_bpm", "threshold_pace_running_s_per_km", "css_swimming_s_per_100m"}

// Context assembles the latest value per context key as of asOf: stored
// measurements first, the dated athlete profile for the keys without one.
func (s *Store) Context(ctx context.Context, userID domain.UserID, asOf *time.Time) (map[string]domain.Metric, error) {
	latest, err := s.Latest(ctx, userID, ContextKeys, asOf)
	if err != nil {
		return nil, err
	}
	if s.Athlete == nil {
		return latest, nil
	}
	entries, err := s.Athlete.ListEntries(ctx, userID)
	if err != nil {
		return nil, internalErr("athlete profile", err)
	}
	at := s.now()
	if asOf != nil {
		at = *asOf
	}
	profile := domain.NewAthleteProfile(entries)
	for athleteKey, metricKeys := range athleteContextKeys {
		v, ok := profile.ValueAt(athleteKey, at)
		if !ok {
			continue
		}
		for _, key := range metricKeys {
			if _, have := latest[key]; have {
				continue
			}
			value := v
			latest[key] = domain.Metric{UserID: userID, Key: key, Timestamp: at.UTC().Truncate(24 * time.Hour), ValueNumeric: &value, Provider: domain.MetricProviderManual}
		}
	}
	return latest, nil
}
