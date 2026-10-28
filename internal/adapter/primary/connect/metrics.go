package connect

import (
	"context"
	"fmt"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// MetricsServer implements the backed parts of cairnv1connect.MetricsServiceHandler:
// the raw metric list, the health series and the athlete physiology profile.
type MetricsServer struct {
	cairnv1connect.UnimplementedMetricsServiceHandler

	Metrics port.MetricRepo         // nil → ListMetrics Unavailable
	Health  *view.Health            // nil → ListHealthSeries Unavailable
	Athlete port.AthleteProfileRepo // nil → athlete RPCs Unavailable
}

var _ cairnv1connect.MetricsServiceHandler = (*MetricsServer)(nil)

// NewMetricsServer wires the server; every dependency may be nil.
func NewMetricsServer(metrics port.MetricRepo, health *view.Health, athlete port.AthleteProfileRepo) *MetricsServer {
	return &MetricsServer{Metrics: metrics, Health: health, Athlete: athlete}
}

// metricsDefaultWindow bounds ListMetrics when no time range is given.
const metricsDefaultWindow = 365 * 24 * time.Hour

// ListMetrics returns the caller's metrics in a window, one key at a time
// (the repo filters by a single key).
func (s *MetricsServer) ListMetrics(ctx context.Context, req *v1.ListMetricsRequest) (*v1.ListMetricsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Metrics == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "metrics are not available on this instance")
	}
	key, start, end, err := listMetricsQuery(req)
	if err != nil {
		return nil, err
	}
	metrics, err := s.Metrics.ListMetricsForUser(ctx, userID, key, start, end)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("list metrics: %w", err))
	}
	if req.GetSort() == v1.MetricSort_METRIC_SORT_TS_DESC {
		for i, j := 0, len(metrics)-1; i < j; i, j = i+1, j-1 {
			metrics[i], metrics[j] = metrics[j], metrics[i]
		}
	}
	return pageMetrics(metrics, req.GetPage()), nil
}

// listMetricsQuery validates the filters and resolves the window.
func listMetricsQuery(req *v1.ListMetricsRequest) (key string, start, end time.Time, err error) {
	if len(req.GetKeys()) > 1 {
		return "", start, end, connectrpc.NewError(connectrpc.CodeInvalidArgument, "one key at a time")
	}
	if len(req.GetProviders()) > 0 || req.ExternalAccountId != nil || req.ActivityId != nil {
		return "", start, end, connectrpc.NewError(connectrpc.CodeInvalidArgument, "providers, external_account_id and activity_id filters are not supported")
	}
	if len(req.GetKeys()) == 1 {
		key = req.GetKeys()[0]
	}
	now := time.Now().UTC()
	start, end = now.Add(-metricsDefaultWindow), now
	if tr := req.GetTimeRange(); tr != nil {
		if tr.GetFrom() != nil {
			start = tr.GetFrom().AsTime()
		}
		if tr.GetTo() != nil {
			end = tr.GetTo().AsTime()
		}
	}
	return key, start, end, nil
}

// pageMetrics applies the offset cursor to the full result.
func pageMetrics(metrics []domain.Metric, page *v1.PageRequest) *v1.ListMetricsResponse {
	limit, offset := pageFrom(page)
	out := &v1.ListMetricsResponse{Page: &v1.PageResponse{Total: int64(len(metrics))}}
	if offset >= len(metrics) {
		return out
	}
	end := min(offset+limit, len(metrics))
	for _, m := range metrics[offset:end] {
		out.Metrics = append(out.Metrics, metricToProto(m))
	}
	if end < len(metrics) {
		out.Page.NextCursor = encodeOffset(end)
	}
	return out
}

func metricToProto(m domain.Metric) *v1.Metric {
	out := &v1.Metric{
		Id:            m.ID.String(),
		UserId:        m.UserID.String(),
		Key:           m.Key,
		Ts:            timestamppb.New(m.Timestamp),
		PeriodSeconds: connectClampInt32(m.PeriodSeconds),
		ValueNumeric:  m.ValueNumeric,
		Provider:      m.Provider,
	}
	if len(m.ValueStruct) > 0 {
		var st structpb.Struct
		if err := st.UnmarshalJSON(m.ValueStruct); err == nil {
			out.ValueStruct = &st
		}
	}
	if m.ExternalAccountID != nil {
		v := m.ExternalAccountID.String()
		out.ExternalAccountId = &v
	}
	if m.ExternalID != "" {
		v := m.ExternalID
		out.ExternalId = &v
	}
	if m.SourceWorkerName != "" {
		v := m.SourceWorkerName
		out.SourceWorkerName = &v
	}
	if m.SourceWorkerVersion != "" {
		v := m.SourceWorkerVersion
		out.SourceWorkerVersion = &v
	}
	if m.ActivityID != nil {
		v := m.ActivityID.String()
		out.ActivityId = &v
	}
	return out
}

func (s *MetricsServer) ListHealthSeries(ctx context.Context, req *v1.ListHealthSeriesRequest) (*v1.ListHealthSeriesResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Health == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "health data is not available on this instance")
	}
	merged, err := s.Health.Series(ctx, userID, req.GetType(), int(req.GetDays()))
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListHealthSeriesResponse{Type: req.GetType()}
	for _, m := range merged {
		out.Series = append(out.Series, &v1.HealthSample{Day: m.Day.Format("2006-01-02"), Value: m.Value, Unit: m.Unit, Provider: m.Provider})
	}
	return out, nil
}

func (s *MetricsServer) athlete() (port.AthleteProfileRepo, error) {
	if s.Athlete == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "the athlete profile is not available on this instance")
	}
	return s.Athlete, nil
}

func (s *MetricsServer) ListAthleteMetrics(ctx context.Context, _ *v1.ListAthleteMetricsRequest) (*v1.ListAthleteMetricsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := s.athlete()
	if err != nil {
		return nil, err
	}
	entries, err := repo.ListEntries(ctx, userID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("list athlete metrics: %w", err))
	}
	out := &v1.ListAthleteMetricsResponse{}
	for _, sp := range domain.AthleteMetricSpecs {
		out.Specs = append(out.Specs, &v1.AthleteMetricSpec{Key: string(sp.Key), Label: sp.Label, Unit: sp.Unit, Min: sp.Min, Max: sp.Max})
	}
	for _, e := range entries {
		out.Entries = append(out.Entries, &v1.AthleteMetricEntry{Id: e.ID.String(), Key: string(e.Key), EffectiveDate: e.EffectiveDate.UTC().Format("2006-01-02"), Value: e.Value})
	}
	return out, nil
}

func (s *MetricsServer) UpsertAthleteMetric(ctx context.Context, req *v1.UpsertAthleteMetricRequest) (*v1.UpsertAthleteMetricResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := s.athlete()
	if err != nil {
		return nil, err
	}
	day, err := time.Parse("2006-01-02", req.GetEffectiveDate())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "effective_date must be YYYY-MM-DD")
	}
	entry := domain.AthleteMetricEntry{UserID: userID, Key: domain.AthleteMetricKey(req.GetKey()), EffectiveDate: day.UTC(), Value: req.GetValue()}
	if err := entry.Validate(); err != nil {
		return nil, connectError(connectrpc.CodeInvalidArgument, err)
	}
	id, err := repo.UpsertEntry(ctx, entry)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("upsert athlete metric: %w", err))
	}
	return &v1.UpsertAthleteMetricResponse{Id: id.String()}, nil
}

func (s *MetricsServer) DeleteAthleteMetric(ctx context.Context, req *v1.DeleteAthleteMetricRequest) (*v1.DeleteAthleteMetricResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := s.athlete()
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid id")
	}
	if err := repo.DeleteEntry(ctx, userID, domain.AthleteMetricID(id)); err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("delete athlete metric: %w", err))
	}
	return &v1.DeleteAthleteMetricResponse{}, nil
}
