package connect

import (
	"context"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/fleet"
)

// Worker fleet: live workers, pools and enrollments.

func (s *AdminServer) fleet() error {
	if s.Fleet == nil {
		return unavailable("the worker fleet")
	}
	return nil
}

func (s *AdminServer) ListWorkers(ctx context.Context, _ *v1.ListWorkersRequest) (*v1.ListWorkersResponse, error) {
	if err := s.fleet(); err != nil {
		return nil, err
	}
	out := &v1.ListWorkersResponse{}
	for _, w := range s.Fleet.Workers(ctx) {
		out.Workers = append(out.Workers, workerHeartbeatToProto(w, s.CoreRelease))
	}
	return out, nil
}

func (s *AdminServer) ListWorkerPools(ctx context.Context, _ *v1.ListWorkerPoolsRequest) (*v1.ListWorkerPoolsResponse, error) {
	if err := s.fleet(); err != nil {
		return nil, err
	}
	pools, err := s.Fleet.PoolStatuses(ctx)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListWorkerPoolsResponse{}
	for _, p := range pools {
		row := &v1.WorkerPool{Provider: p.Provider, Name: p.Name, Package: p.Package, FirstSeenAt: timestamppb.New(p.FirstSeenAt),
			LastSeenAt: timestamppb.New(p.LastSeenAt), OnlineInstances: connectClampInt32(p.OnlineInstances)}
		if p.RetiredAt != nil {
			row.RetiredAt = timestamppb.New(*p.RetiredAt)
		}
		out.Pools = append(out.Pools, row)
	}
	return out, nil
}

func (s *AdminServer) RetireWorkerPool(ctx context.Context, req *v1.RetireWorkerPoolRequest) (*v1.RetireWorkerPoolResponse, error) {
	if err := s.fleet(); err != nil {
		return nil, err
	}
	if err := s.Fleet.RetirePool(ctx, s.actor(ctx), req.GetProvider(), req.GetName()); err != nil {
		return nil, actionErr(err)
	}
	return &v1.RetireWorkerPoolResponse{}, nil
}

func (s *AdminServer) ListWorkerEnrollments(ctx context.Context, req *v1.ListWorkerEnrollmentsRequest) (*v1.ListWorkerEnrollmentsResponse, error) {
	if err := s.fleet(); err != nil {
		return nil, err
	}
	list, err := s.Fleet.ListEnrollments(ctx, req.GetProvider(), req.GetIncludeRevoked(), req.GetIncludeExpired())
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListWorkerEnrollmentsResponse{Providers: s.Fleet.Providers}
	for _, e := range list {
		out.Enrollments = append(out.Enrollments, enrollmentToProto(e))
	}
	return out, nil
}

func (s *AdminServer) CreateWorkerEnrollment(ctx context.Context, req *v1.CreateWorkerEnrollmentRequest) (*v1.CreateWorkerEnrollmentResponse, error) {
	if err := s.fleet(); err != nil {
		return nil, err
	}
	in := fleet.EnrollmentInput{Name: req.GetName(), Provider: req.GetProvider(), Note: req.GetNote()}
	if req.ExpiresIn != nil {
		in.ExpiresIn = req.GetExpiresIn().AsDuration()
	}
	e, token, err := s.Fleet.CreateEnrollment(ctx, s.actor(ctx), in)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.CreateWorkerEnrollmentResponse{Enrollment: enrollmentToProto(e), Token: token}, nil
}

func parseEnrollmentID(raw string) (domain.WorkerEnrollmentID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return domain.WorkerEnrollmentID{}, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad enrollment id")
	}
	return domain.WorkerEnrollmentID(id), nil
}

func (s *AdminServer) RevokeWorkerEnrollment(ctx context.Context, req *v1.RevokeWorkerEnrollmentRequest) (*v1.RevokeWorkerEnrollmentResponse, error) {
	if err := s.fleet(); err != nil {
		return nil, err
	}
	id, err := parseEnrollmentID(req.GetId())
	if err != nil {
		return nil, err
	}
	if err := s.Fleet.RevokeEnrollment(ctx, s.actor(ctx), id, req.GetReason()); err != nil {
		return nil, actionErr(err)
	}
	return &v1.RevokeWorkerEnrollmentResponse{}, nil
}

func (s *AdminServer) ProlongWorkerEnrollment(ctx context.Context, req *v1.ProlongWorkerEnrollmentRequest) (*v1.ProlongWorkerEnrollmentResponse, error) {
	if err := s.fleet(); err != nil {
		return nil, err
	}
	id, err := parseEnrollmentID(req.GetId())
	if err != nil {
		return nil, err
	}
	expires, err := s.Fleet.ProlongEnrollment(ctx, s.actor(ctx), id, req.GetExpiresIn().AsDuration())
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.ProlongWorkerEnrollmentResponse{ExpiresAt: timestamppb.New(expires)}, nil
}

func workerHeartbeatToProto(w domain.WorkerHeartbeat, coreRelease string) *v1.LiveWorker {
	out := &v1.LiveWorker{
		WorkerName: w.WorkerName, WorkerKey: w.WorkerKey, InstanceId: w.InstanceID, Version: w.Version, Release: w.Release,
		ReleaseCompatible: w.Compatible, ReleaseKnown: w.Known, CoreRelease: coreRelease, Provider: w.Provider, Package: w.Package,
		GoVersion: w.GoVersion, Commit: w.Commit, BuildDate: w.BuildDate, Webhooks: w.Webhooks, Capabilities: map[string]*v1.WorkerCapability{},
	}
	for k, c := range w.Capabilities {
		out.Capabilities[k] = &v1.WorkerCapability{Read: c.Read, Write: c.Write, Backfill: c.Backfill, Granularity: c.Granularity}
	}
	if !w.LastSeen.IsZero() {
		out.LastSeen = timestamppb.New(w.LastSeen)
	}
	return out
}

func enrollmentToProto(e domain.WorkerEnrollment) *v1.WorkerEnrollment {
	out := &v1.WorkerEnrollment{
		Id: e.ID.String(), Provider: e.Provider, Name: e.Name, Version: connectClampInt32(e.Version), WorkerKey: e.WorkerKey(),
		WorkerNamePattern: e.WorkerNamePattern, PermissionTemplate: e.PermissionTemplate, Note: e.Note,
		CreatedAt: timestamppb.New(e.CreatedAt), ExpiresAt: timestamppb.New(e.ExpiresAt), MaxUses: connectClampInt32(e.MaxUses), Uses: connectClampInt32(e.Uses),
		RevokedReason: e.RevokedReason, IsRevoked: e.IsRevoked(), ReenrollRequired: e.NeedsReenroll(),
	}
	if e.CreatedByUserID != nil {
		v := e.CreatedByUserID.String()
		out.CreatedByUserId = &v
	}
	if e.RevokedAt != nil {
		out.RevokedAt = timestamppb.New(*e.RevokedAt)
	}
	if e.RevokedByUserID != nil {
		v := e.RevokedByUserID.String()
		out.RevokedByUserId = &v
	}
	return out
}
