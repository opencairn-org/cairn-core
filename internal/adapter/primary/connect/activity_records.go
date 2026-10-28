package connect

import (
	"context"
	"fmt"

	connectrpc "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ListRecords returns the caller's all-time personal records per (activity
// type, metric, window), the same data as GET /api/records. activity_type and
// metrics filter the set; discipline and time_range have no repo support yet
// and are refused rather than silently ignored.
func (s *ActivityServer) ListRecords(ctx context.Context, req *v1.ListRecordsRequest) (*v1.ListRecordsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	keep, err := recordFilter(req)
	if err != nil {
		return nil, err
	}
	recs, err := s.BestEfforts.ListPersonalRecords(ctx, userID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("list personal records: %w", err))
	}
	out := &v1.ListRecordsResponse{Records: make([]*v1.UserRecord, 0, len(recs))}
	for _, r := range recs {
		if keep(r) {
			out.Records = append(out.Records, personalRecordToProto(userID, r))
		}
	}
	return out, nil
}

// recordFilter builds the predicate for the request's supported filters.
func recordFilter(req *v1.ListRecordsRequest) (func(domain.PersonalRecord) bool, error) {
	if req.Discipline != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "discipline filter is not supported")
	}
	if req.TimeRange != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "time_range filter is not supported")
	}
	var typeFilter *domain.ActivityType
	if req.GetActivityType() != v1.ActivityType_ACTIVITY_TYPE_UNSPECIFIED {
		t, ok := activityTypeFromProto(req.GetActivityType())
		if !ok {
			return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "unknown activity_type")
		}
		typeFilter = &t
	}
	allowed := metricFilter(req.GetMetrics())
	return func(r domain.PersonalRecord) bool {
		if typeFilter != nil && r.ActivityType != *typeFilter {
			return false
		}
		if allowed == nil {
			return true
		}
		_, ok := allowed[r.Metric]
		return ok
	}, nil
}

func personalRecordToProto(userID domain.UserID, r domain.PersonalRecord) *v1.UserRecord {
	return &v1.UserRecord{
		UserId:        userID.String(),
		ActivityType:  activityTypeToProto(r.ActivityType),
		Metric:        bestEffortMetricToProto(r.Metric),
		WindowKind:    bestEffortWindowKindToProto(r.WindowKind),
		WindowValue:   connectClampInt32(r.WindowValue),
		AchievedValue: r.AchievedValue,
		ActivityId:    r.ActivityID.String(),
		Ts:            timestamppb.New(r.Timestamp),
	}
}
