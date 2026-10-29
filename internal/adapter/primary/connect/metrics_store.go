package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/metrics"
)

// The metric store RPCs: latest values, charting series, manual entry and
// the training context. Each is a thin shell over usecase/metrics.Store.

func (s *MetricsServer) GetLatestMetrics(ctx context.Context, req *v1.GetLatestMetricsRequest) (*v1.GetLatestMetricsResponse, error) {
	userID, store, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	latest, err := store.Latest(ctx, userID, req.GetKeys(), optionalTime(req.GetAsOf()))
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.GetLatestMetricsResponse{Metrics: metricsMapToProto(latest)}, nil
}

func (s *MetricsServer) QueryMetricSeries(ctx context.Context, req *v1.QueryMetricSeriesRequest) (*v1.QueryMetricSeriesResponse, error) {
	userID, store, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	q := domain.MetricSeriesQuery{Key: req.GetKey(), Bucket: metricBuckets[req.GetBucket()], Aggregation: metricAggregations[req.GetAggregation()],
		Providers: req.GetProviders(), MaxPoints: int(req.GetMaxPoints())}
	q.Start, q.End = timeRangeBounds(req.GetTimeRange())
	points, err := store.Series(ctx, userID, q)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.QueryMetricSeriesResponse{Key: q.Key, Bucket: req.GetBucket(), Aggregation: metricAggregationToProto(q.Aggregation)}
	for _, p := range points {
		out.Points = append(out.Points, &v1.MetricSeriesPoint{Ts: timestamppb.New(p.Ts), Value: p.Value, SampleCount: connectClampInt32(p.Samples)})
	}
	return out, nil
}

func (s *MetricsServer) UpsertMetric(ctx context.Context, req *v1.UpsertMetricRequest) (*v1.UpsertMetricResponse, error) {
	userID, store, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	in, err := upsertInputFromProto(req)
	if err != nil {
		return nil, err
	}
	m, _, err := store.Upsert(ctx, userID, in)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.UpsertMetricResponse{Metric: metricToProto(m)}, nil
}

func (s *MetricsServer) DeleteMetric(ctx context.Context, req *v1.DeleteMetricRequest) (*v1.DeleteMetricResponse, error) {
	userID, store, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid id")
	}
	if err := store.Delete(ctx, userID, domain.MetricID(id)); err != nil {
		return nil, actionErr(err)
	}
	return &v1.DeleteMetricResponse{}, nil
}

func (s *MetricsServer) BulkUpsertMetrics(ctx context.Context, req *v1.BulkUpsertMetricsRequest) (*v1.BulkUpsertMetricsResponse, error) {
	userID, store, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]metrics.UpsertInput, 0, len(req.GetMetrics()))
	for i, row := range req.GetMetrics() {
		in, err := upsertInputFromProto(row)
		if err != nil {
			return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, fmt.Sprintf("row %d: %v", i, err))
		}
		rows = append(rows, in)
	}
	res, err := store.BulkUpsert(ctx, userID, rows, req.GetContinueOnError())
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.BulkUpsertMetricsResponse{Created: connectClampInt32(res.Created), Updated: connectClampInt32(res.Updated), Skipped: connectClampInt32(res.Skipped)}
	for _, e := range res.Errors {
		out.Errors = append(out.Errors, &v1.BulkUpsertError{RowIndex: connectClampInt32(e.Row), Code: e.Code, Message: e.Message})
	}
	return out, nil
}

func (s *MetricsServer) GetUserContext(ctx context.Context, req *v1.GetUserContextRequest) (*v1.GetUserContextResponse, error) {
	userID, store, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	latest, err := store.Context(ctx, userID, optionalTime(req.GetAsOf()))
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.GetUserContextResponse{LatestMetrics: metricsMapToProto(latest), Settings: &v1.UserSettings{UserId: userID.String()}}
	if s.Settings != nil {
		policies, err := s.Settings.GetAllMergePolicies(ctx, userID)
		if err != nil {
			return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("merge policies: %w", err))
		}
		out.Settings.MergePolicyByActivityType = mergePoliciesToProto(policies)
	}
	return out, nil
}

// caller resolves the session user and the wired store.
func (s *MetricsServer) caller(ctx context.Context) (domain.UserID, *metrics.Store, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return userID, nil, err
	}
	store, err := s.store()
	return userID, store, err
}

func optionalTime(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}

func metricsMapToProto(m map[string]domain.Metric) map[string]*v1.Metric {
	out := make(map[string]*v1.Metric, len(m))
	for k, v := range m {
		out[k] = metricToProto(v)
	}
	return out
}

func upsertInputFromProto(req *v1.UpsertMetricRequest) (metrics.UpsertInput, error) {
	in := metrics.UpsertInput{Key: req.GetKey(), PeriodSeconds: int(req.GetPeriodSeconds()), ValueNumeric: req.ValueNumeric,
		Provider: req.GetProvider(), Tags: req.GetTags(), Notes: req.GetNotes(), DedupByNaturalKey: req.GetDedupByNaturalKey()}
	if req.GetId() != "" {
		id, err := uuid.Parse(req.GetId())
		if err != nil {
			return in, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid id")
		}
		mid := domain.MetricID(id)
		in.ID = &mid
	}
	if req.GetTs() != nil {
		in.Ts = req.GetTs().AsTime()
	}
	if req.ValueStruct != nil {
		raw, err := req.GetValueStruct().MarshalJSON()
		if err != nil {
			return in, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid value_struct")
		}
		in.ValueStruct = json.RawMessage(raw)
	}
	return in, nil
}

var metricBuckets = map[v1.MetricBucket]domain.MetricBucket{
	v1.MetricBucket_METRIC_BUCKET_HOUR: domain.MetricBucketHour, v1.MetricBucket_METRIC_BUCKET_DAY: domain.MetricBucketDay,
	v1.MetricBucket_METRIC_BUCKET_WEEK: domain.MetricBucketWeek, v1.MetricBucket_METRIC_BUCKET_MONTH: domain.MetricBucketMonth,
}

var metricAggregations = map[v1.MetricAggregation]domain.MetricAggregation{
	v1.MetricAggregation_METRIC_AGGREGATION_AVG: domain.MetricAggAvg, v1.MetricAggregation_METRIC_AGGREGATION_MIN: domain.MetricAggMin,
	v1.MetricAggregation_METRIC_AGGREGATION_MAX: domain.MetricAggMax, v1.MetricAggregation_METRIC_AGGREGATION_SUM: domain.MetricAggSum,
	v1.MetricAggregation_METRIC_AGGREGATION_LAST: domain.MetricAggLast, v1.MetricAggregation_METRIC_AGGREGATION_FIRST: domain.MetricAggFirst,
	v1.MetricAggregation_METRIC_AGGREGATION_COUNT: domain.MetricAggCount,
}

func metricAggregationToProto(a domain.MetricAggregation) v1.MetricAggregation {
	for p, d := range metricAggregations {
		if d == a {
			return p
		}
	}
	return v1.MetricAggregation_METRIC_AGGREGATION_UNSPECIFIED
}
