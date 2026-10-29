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
	"github.com/opencairn-org/cairn-core/internal/usecase/metrics"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// MetricsServer implements cairnv1connect.MetricsServiceHandler: the metric
// store, the health series and the athlete physiology profile.
type MetricsServer struct {
	cairnv1connect.UnimplementedMetricsServiceHandler

	Store    *metrics.Store          // nil → metric RPCs Unavailable
	Health   *view.Health            // nil → ListHealthSeries Unavailable
	Athlete  port.AthleteProfileRepo // nil → athlete RPCs Unavailable
	Settings port.UserSettingsRepo   // nil → GetUserContext carries no settings
}

var _ cairnv1connect.MetricsServiceHandler = (*MetricsServer)(nil)

// NewMetricsServer wires the server; every dependency may be nil.
func NewMetricsServer(store *metrics.Store, health *view.Health, athlete port.AthleteProfileRepo, settings port.UserSettingsRepo) *MetricsServer {
	return &MetricsServer{Store: store, Health: health, Athlete: athlete, Settings: settings}
}

func (s *MetricsServer) store() (*metrics.Store, error) {
	if s.Store == nil {
		return nil, unavailable("metrics")
	}
	return s.Store, nil
}

// ListMetrics pages the caller's metrics by the request filters.
func (s *MetricsServer) ListMetrics(ctx context.Context, req *v1.ListMetricsRequest) (*v1.ListMetricsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	f, err := listMetricsFilter(req)
	if err != nil {
		return nil, err
	}
	rows, total, err := store.List(ctx, userID, f)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListMetricsResponse{Page: &v1.PageResponse{Total: int64(total)}}
	for _, m := range rows {
		out.Metrics = append(out.Metrics, metricToProto(m))
	}
	if f.Offset+len(rows) < total {
		out.Page.NextCursor = encodeOffset(f.Offset + len(rows))
	}
	return out, nil
}

// listMetricsFilter converts the request into the use-case filter.
func listMetricsFilter(req *v1.ListMetricsRequest) (domain.MetricListFilter, error) {
	f := domain.MetricListFilter{Keys: req.GetKeys(), Providers: req.GetProviders(), Ascending: req.GetSort() == v1.MetricSort_METRIC_SORT_TS_ASC}
	f.Limit, f.Offset = pageFrom(req.GetPage())
	f.Start, f.End = timeRangeBounds(req.GetTimeRange())
	if req.ExternalAccountId != nil {
		id, err := uuid.Parse(req.GetExternalAccountId())
		if err != nil {
			return f, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid external_account_id")
		}
		acct := domain.ExternalAccountID(id)
		f.ExternalAccountID = &acct
	}
	if req.ActivityId != nil {
		id, err := uuid.Parse(req.GetActivityId())
		if err != nil {
			return f, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid activity_id")
		}
		act := domain.ActivityID(id)
		f.ActivityID = &act
	}
	return f, nil
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
