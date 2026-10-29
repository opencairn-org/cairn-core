package connect

import (
	"context"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Moderation, quotas, federation blocks, merge defaults and operations.

func (s *AdminServer) ListReports(ctx context.Context, req *v1.ListReportsRequest) (*v1.ListReportsResponse, error) {
	if s.Reports == nil {
		return nil, unavailable("reports")
	}
	limit, offset := pageFrom(req.GetPage())
	rows, err := s.Reports.List(ctx, reportStatusFromProto(req.GetStatus()), limit, offset)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListReportsResponse{Page: &v1.PageResponse{Total: -1}}
	for _, r := range rows {
		row := &v1.ContentReport{Id: r.ID.String(), ReporterUserId: r.ReporterID.String(), ReporterUsername: r.ReporterUsername,
			TargetKind: string(r.TargetKind), TargetId: r.TargetID.String(), Reason: r.Reason, Status: reportStatusToProto(r.Status), CreatedAt: timestamppb.New(r.CreatedAt)}
		if r.ReviewedAt != nil {
			row.ReviewedAt = timestamppb.New(*r.ReviewedAt)
		}
		if r.ReviewedBy != nil {
			v := r.ReviewedBy.String()
			row.ReviewedByUserId = &v
		}
		out.Reports = append(out.Reports, row)
	}
	if len(rows) == limit {
		out.Page.NextCursor = encodeOffset(offset + limit)
	}
	return out, nil
}

func (s *AdminServer) ResolveReport(ctx context.Context, req *v1.ResolveReportRequest) (*v1.ResolveReportResponse, error) {
	if s.Reports == nil {
		return nil, unavailable("reports")
	}
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad report id")
	}
	status, err := s.Reports.Resolve(ctx, s.actor(ctx), domain.ContentReportID(id), reportStatusFromProto(req.GetStatus()))
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.ResolveReportResponse{Status: reportStatusToProto(status)}, nil
}

func (s *AdminServer) SetActivityHidden(ctx context.Context, req *v1.SetActivityHiddenRequest) (*v1.SetActivityHiddenResponse, error) {
	if s.Reports == nil {
		return nil, unavailable("moderation")
	}
	id, err := uuid.Parse(req.GetActivityId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad activity id")
	}
	if err := s.Reports.SetActivityHidden(ctx, s.actor(ctx), id, req.GetHidden()); err != nil {
		return nil, actionErr(err)
	}
	return &v1.SetActivityHiddenResponse{}, nil
}

func (s *AdminServer) SetUserQuota(ctx context.Context, req *v1.SetUserQuotaRequest) (*v1.SetUserQuotaResponse, error) {
	if err := s.userAdmin(); err != nil {
		return nil, err
	}
	id, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, err
	}
	var maxActivities *int
	if req.MaxActivities != nil {
		n := int(req.GetMaxActivities())
		maxActivities = &n
	}
	if err := s.UserAdmin.SetQuota(ctx, s.actor(ctx), id, maxActivities); err != nil {
		return nil, actionErr(err)
	}
	return &v1.SetUserQuotaResponse{}, nil
}

func (s *AdminServer) ListBlockedDomains(ctx context.Context, _ *v1.ListBlockedDomainsRequest) (*v1.ListBlockedDomainsResponse, error) {
	if s.Blocks == nil {
		return nil, unavailable("federation")
	}
	list, err := s.Blocks.List(ctx)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListBlockedDomainsResponse{}
	for _, b := range list {
		out.Domains = append(out.Domains, &v1.BlockedDomain{Domain: b.Domain, Reason: b.Reason, CreatedAt: timestamppb.New(b.CreatedAt)})
	}
	return out, nil
}

func (s *AdminServer) BlockDomain(ctx context.Context, req *v1.BlockDomainRequest) (*v1.BlockDomainResponse, error) {
	if s.Blocks == nil {
		return nil, unavailable("federation")
	}
	d, err := s.Blocks.Block(ctx, s.actor(ctx), req.GetDomain(), req.GetReason())
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.BlockDomainResponse{Domain: &v1.BlockedDomain{Domain: d, Reason: req.GetReason(), CreatedAt: timestamppb.Now()}}, nil
}

func (s *AdminServer) UnblockDomain(ctx context.Context, req *v1.UnblockDomainRequest) (*v1.UnblockDomainResponse, error) {
	if s.Blocks == nil {
		return nil, unavailable("federation")
	}
	if err := s.Blocks.Unblock(ctx, s.actor(ctx), req.GetDomain()); err != nil {
		return nil, actionErr(err)
	}
	return &v1.UnblockDomainResponse{}, nil
}

func (s *AdminServer) GetInstanceMergeDefaults(ctx context.Context, _ *v1.GetInstanceMergeDefaultsRequest) (*v1.GetInstanceMergeDefaultsResponse, error) {
	if s.MergeDefaults == nil {
		return nil, unavailable("merge defaults")
	}
	policies, groups, err := s.MergeDefaults.Get(ctx)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.GetInstanceMergeDefaultsResponse{Policies: mergePoliciesToProto(policies), FieldGroups: groups}, nil
}

func (s *AdminServer) UpdateInstanceMergeDefaults(ctx context.Context, req *v1.UpdateInstanceMergeDefaultsRequest) (*v1.UpdateInstanceMergeDefaultsResponse, error) {
	if s.MergeDefaults == nil {
		return nil, unavailable("merge defaults")
	}
	policies, err := mergePoliciesFromProto(req.GetPolicies())
	if err != nil {
		return nil, err
	}
	if err := s.MergeDefaults.Set(ctx, s.actor(ctx), policies); err != nil {
		return nil, actionErr(err)
	}
	return &v1.UpdateInstanceMergeDefaultsResponse{}, nil
}

func (s *AdminServer) RunGeocodeBackfill(ctx context.Context, req *v1.RunGeocodeBackfillRequest) (*v1.RunGeocodeBackfillResponse, error) {
	if s.Ops == nil {
		return nil, unavailable("operations")
	}
	limit, err := s.Ops.RunGeocodeBackfill(ctx, s.actor(ctx), int(req.GetLimit()))
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.RunGeocodeBackfillResponse{Started: true, Limit: connectClampInt32(limit)}, nil
}

func (s *AdminServer) RebuildGearTotals(ctx context.Context, _ *v1.RebuildGearTotalsRequest) (*v1.RebuildGearTotalsResponse, error) {
	if s.Ops == nil {
		return nil, unavailable("operations")
	}
	n, err := s.Ops.RebuildGearTotals(ctx, s.actor(ctx))
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.RebuildGearTotalsResponse{Users: connectClampInt32(n)}, nil
}

func (s *AdminServer) BootstrapNATSAccountKey(ctx context.Context, _ *v1.BootstrapNATSAccountKeyRequest) (*v1.BootstrapNATSAccountKeyResponse, error) {
	if s.Ops == nil {
		return nil, unavailable("operations")
	}
	key, created, err := s.Ops.BootstrapKey(ctx, s.actor(ctx))
	if err != nil {
		return nil, actionErr(err)
	}
	action := "no_change"
	if created {
		action = "created"
	}
	return &v1.BootstrapNATSAccountKeyResponse{PublicKey: key.PublicKey, CreatedAt: timestamppb.New(key.CreatedAt), Action: action}, nil
}

func (s *AdminServer) RotateNATSAccountKey(ctx context.Context, _ *v1.RotateNATSAccountKeyRequest) (*v1.RotateNATSAccountKeyResponse, error) {
	if s.Ops == nil {
		return nil, unavailable("operations")
	}
	key, old, err := s.Ops.RotateKey(ctx, s.actor(ctx))
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.RotateNATSAccountKeyResponse{NewPublicKey: key.PublicKey, OldPublicKey: old}, nil
}

func reportStatusToProto(s domain.ReportStatus) v1.ReportStatus {
	switch s {
	case domain.ReportOpen:
		return v1.ReportStatus_REPORT_STATUS_OPEN
	case domain.ReportReviewed:
		return v1.ReportStatus_REPORT_STATUS_REVIEWED
	case domain.ReportDismissed:
		return v1.ReportStatus_REPORT_STATUS_DISMISSED
	}
	return v1.ReportStatus_REPORT_STATUS_UNSPECIFIED
}

func reportStatusFromProto(s v1.ReportStatus) domain.ReportStatus {
	switch s {
	case v1.ReportStatus_REPORT_STATUS_OPEN:
		return domain.ReportOpen
	case v1.ReportStatus_REPORT_STATUS_REVIEWED:
		return domain.ReportReviewed
	case v1.ReportStatus_REPORT_STATUS_DISMISSED:
		return domain.ReportDismissed
	}
	return ""
}
