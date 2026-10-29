package connect

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

func (s *AdminServer) TriggerMaintenance(ctx context.Context, req *v1.TriggerMaintenanceRequest) (*v1.TriggerMaintenanceResponse, error) {
	if s.Maintenance == nil {
		return nil, unavailable("maintenance")
	}
	users := make([]domain.UserID, 0, len(req.GetUserIds()))
	for _, raw := range req.GetUserIds() {
		uid, err := parseUserID(raw)
		if err != nil {
			return nil, err
		}
		users = append(users, uid)
	}
	job, err := s.Maintenance.Trigger(ctx, s.actor(ctx), maintenanceOpFromProto(req.GetOperation()), users, req.GetConfirmPhrase())
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.TriggerMaintenanceResponse{JobId: job.ID, EstimatedRecordCount: job.Estimated}, nil
}

func (s *AdminServer) ListMaintenanceJobs(_ context.Context, _ *v1.ListMaintenanceJobsRequest) (*v1.ListMaintenanceJobsResponse, error) {
	if s.Maintenance == nil {
		return nil, unavailable("maintenance")
	}
	out := &v1.ListMaintenanceJobsResponse{}
	for _, job := range s.Maintenance.List() {
		out.Jobs = append(out.Jobs, maintenanceJobToProto(job))
	}
	return out, nil
}

func (s *AdminServer) CancelMaintenanceJob(ctx context.Context, req *v1.CancelMaintenanceJobRequest) (*v1.CancelMaintenanceJobResponse, error) {
	if s.Maintenance == nil {
		return nil, unavailable("maintenance")
	}
	job, err := s.Maintenance.Cancel(ctx, s.actor(ctx), req.GetJobId())
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.CancelMaintenanceJobResponse{Job: maintenanceJobToProto(job)}, nil
}

var maintenanceOps = map[v1.MaintenanceOperation]domain.MaintenanceOperation{
	v1.MaintenanceOperation_MAINTENANCE_OPERATION_REMATCH_ALL_SEGMENTS:      domain.MaintenanceRematchAllSegments,
	v1.MaintenanceOperation_MAINTENANCE_OPERATION_RECOMPUTE_BEST_EFFORTS:    domain.MaintenanceRecomputeBestEfforts,
	v1.MaintenanceOperation_MAINTENANCE_OPERATION_RECOMPUTE_ACTIVITY_MERGES: domain.MaintenanceRecomputeActivityMerges,
	v1.MaintenanceOperation_MAINTENANCE_OPERATION_RECOMPUTE_TRAINING_LOAD:   domain.MaintenanceRecomputeTrainingLoad,
	v1.MaintenanceOperation_MAINTENANCE_OPERATION_PURGE_SOFT_DELETED:        domain.MaintenancePurgeSoftDeleted,
	v1.MaintenanceOperation_MAINTENANCE_OPERATION_GARBAGE_COLLECT_BLOBS:     domain.MaintenanceGarbageCollectBlobs,
}

func maintenanceOpFromProto(op v1.MaintenanceOperation) domain.MaintenanceOperation {
	return maintenanceOps[op]
}

func maintenanceOpToProto(op domain.MaintenanceOperation) v1.MaintenanceOperation {
	for p, d := range maintenanceOps {
		if d == op {
			return p
		}
	}
	return v1.MaintenanceOperation_MAINTENANCE_OPERATION_UNSPECIFIED
}

var maintenanceStatuses = map[domain.MaintenanceStatus]v1.MaintenanceJobStatus{
	domain.MaintenanceQueued:    v1.MaintenanceJobStatus_MAINTENANCE_JOB_STATUS_QUEUED,
	domain.MaintenanceRunning:   v1.MaintenanceJobStatus_MAINTENANCE_JOB_STATUS_RUNNING,
	domain.MaintenanceSucceeded: v1.MaintenanceJobStatus_MAINTENANCE_JOB_STATUS_SUCCEEDED,
	domain.MaintenanceFailed:    v1.MaintenanceJobStatus_MAINTENANCE_JOB_STATUS_FAILED,
	domain.MaintenanceCancelled: v1.MaintenanceJobStatus_MAINTENANCE_JOB_STATUS_CANCELLED,
}

func maintenanceJobToProto(job domain.MaintenanceJob) *v1.MaintenanceJob {
	out := &v1.MaintenanceJob{
		Id: job.ID, Operation: maintenanceOpToProto(job.Operation), Status: maintenanceStatuses[job.Status],
		EstimatedRecordCount: job.Estimated, ProcessedCount: job.Processed, FailedCount: job.Failed, ErrorMessage: job.Error,
		QueuedAt: timestamppb.New(job.QueuedAt),
	}
	for _, uid := range job.UserIDs {
		out.UserIds = append(out.UserIds, uid.String())
	}
	if job.TriggeredBy != nil {
		s := job.TriggeredBy.String()
		out.TriggeredByUserId = &s
	}
	if job.StartedAt != nil {
		out.StartedAt = timestamppb.New(*job.StartedAt)
	}
	if job.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*job.FinishedAt)
	}
	return out
}
