package connect

import (
	"context"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Job introspection over the import queue, the dead-letter queue and the
// orphaned-source view. A queue item is the unit of work core tracks: the
// AdminJob shape maps onto it (attempt counter, timestamps, last error).

func (s *AdminServer) jobs() error {
	if s.Jobs == nil {
		return unavailable("job introspection")
	}
	return nil
}

func parseQueueItemID(raw string) (domain.ImportQueueItemID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return domain.ImportQueueItemID{}, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad job id")
	}
	return domain.ImportQueueItemID(id), nil
}

func (s *AdminServer) ListJobs(ctx context.Context, req *v1.ListJobsRequest) (*v1.ListJobsResponse, error) {
	if err := s.jobs(); err != nil {
		return nil, err
	}
	limit, offset := pageFrom(req.GetPage())
	f := port.ImportQueueFilter{Providers: req.GetProviders(), Limit: limit, Offset: offset}
	for _, st := range req.GetStatuses() {
		f.Statuses = append(f.Statuses, jobStatusFromProto(st)...)
	}
	for _, t := range req.GetJobTypes() {
		f.ItemTypes = append(f.ItemTypes, domain.ImportItemType(t))
	}
	if req.UserId != nil {
		uid, err := parseUserID(req.GetUserId())
		if err != nil {
			return nil, err
		}
		f.UserID = &uid
	}
	f.From, f.To = timeRangeBounds(req.GetEnqueuedRange())
	items, total, counts, err := s.Jobs.List(ctx, f)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListJobsResponse{Page: &v1.PageResponse{Total: int64(total)}, CountByStatus: map[string]int32{}}
	for _, it := range items {
		out.Jobs = append(out.Jobs, adminJobFromQueueItem(it))
	}
	for st, n := range counts {
		out.CountByStatus[jobStatusToProto(st).String()] += connectClampInt32(n)
	}
	if offset+len(items) < total {
		out.Page.NextCursor = encodeOffset(offset + len(items))
	}
	return out, nil
}

func (s *AdminServer) GetJob(ctx context.Context, req *v1.GetJobRequest) (*v1.GetJobResponse, error) {
	if err := s.jobs(); err != nil {
		return nil, err
	}
	id, err := parseQueueItemID(req.GetId())
	if err != nil {
		return nil, err
	}
	it, err := s.Jobs.Get(ctx, id)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.GetJobResponse{Job: adminJobFromQueueItem(it)}
	payload := map[string]any{
		"external_account_id": it.ExternalAccountID.String(), "provider": it.Provider, "item_type": string(it.ItemType),
		"external_id": it.ExternalID, "priority": it.Priority,
	}
	if it.ItemTime != nil {
		payload["item_time"] = it.ItemTime.UTC().Format(time.RFC3339)
	}
	out.PayloadView, _ = structpb.NewStruct(payload)
	if it.StartedAt != nil {
		att := &v1.JobAttempt{Attempt: connectClampInt32(it.Attempts), StartedAt: timestamppb.New(*it.StartedAt), Status: jobStatusToProto(it.Status)}
		if it.CompletedAt != nil {
			att.FinishedAt = timestamppb.New(*it.CompletedAt)
		}
		if it.LastError != "" {
			msg := it.LastError
			att.ErrorMessage = &msg
		}
		out.Attempts = []*v1.JobAttempt{att}
	}
	return out, nil
}

func (s *AdminServer) RetryJob(ctx context.Context, req *v1.RetryJobRequest) (*v1.RetryJobResponse, error) {
	if err := s.jobs(); err != nil {
		return nil, err
	}
	id, err := parseQueueItemID(req.GetId())
	if err != nil {
		return nil, err
	}
	it, err := s.Jobs.Retry(ctx, s.actor(ctx), id, req.GetResetAttempts())
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.RetryJobResponse{Job: adminJobFromQueueItem(it)}, nil
}

func (s *AdminServer) CancelJob(ctx context.Context, req *v1.CancelJobRequest) (*v1.CancelJobResponse, error) {
	if err := s.jobs(); err != nil {
		return nil, err
	}
	id, err := parseQueueItemID(req.GetId())
	if err != nil {
		return nil, err
	}
	it, err := s.Jobs.Cancel(ctx, s.actor(ctx), id)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.CancelJobResponse{Job: adminJobFromQueueItem(it)}, nil
}

func (s *AdminServer) ListDeadLetterJobs(ctx context.Context, req *v1.ListDeadLetterJobsRequest) (*v1.ListDeadLetterJobsResponse, error) {
	if err := s.jobs(); err != nil {
		return nil, err
	}
	limit, _ := pageFrom(req.GetPage())
	from, to := timeRangeBounds(req.GetFailedRange())
	f := port.DeadLetterFilter{Limit: limit, BeforeFirstSeenAt: deadLetterCursor(req.GetPage(), to)}
	list, err := s.Jobs.ListDeadLetters(ctx, f)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListDeadLetterJobsResponse{Page: &v1.PageResponse{Total: -1}}
	for _, j := range list {
		if deadLetterMatches(j, from, req.GetProviders()) {
			out.Jobs = append(out.Jobs, deadLetterToProto(j))
		}
	}
	if len(list) == limit {
		out.Page.NextCursor = list[len(list)-1].FirstSeenAt.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}

// deadLetterCursor is the "before" bound: the page cursor, tightened by the
// range's upper bound.
func deadLetterCursor(p *v1.PageRequest, to time.Time) time.Time {
	var before time.Time
	if p != nil && p.GetCursor() != "" {
		if t, err := time.Parse(time.RFC3339Nano, p.GetCursor()); err == nil {
			before = t
		}
	}
	if !to.IsZero() && (before.IsZero() || to.Before(before)) {
		before = to
	}
	return before
}

func deadLetterMatches(j domain.DeadLetteredJob, from time.Time, providers []string) bool {
	if !from.IsZero() && j.FirstSeenAt.Before(from) {
		return false
	}
	return len(providers) == 0 || subjectHasProvider(j.Subject, providers)
}

func (s *AdminServer) RequeueDeadLetterJob(ctx context.Context, req *v1.RequeueDeadLetterJobRequest) (*v1.RequeueDeadLetterJobResponse, error) {
	if err := s.jobs(); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad id")
	}
	job, _, err := s.Jobs.Requeue(ctx, s.actor(ctx), domain.DeadLetteredJobID(id))
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.RequeueDeadLetterJobResponse{Job: deadLetterToProto(job)}, nil
}

func (s *AdminServer) ListOrphanedSources(ctx context.Context, req *v1.ListOrphanedSourcesRequest) (*v1.ListOrphanedSourcesResponse, error) {
	if s.Orphans == nil {
		return nil, unavailable("orphaned sources")
	}
	groups, err := s.Orphans.List(ctx, req.GetWorkerName())
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListOrphanedSourcesResponse{}
	for _, g := range groups {
		row := &v1.OrphanedSourceGroup{WorkerName: g.WorkerName, SourceCount: connectClampInt32(g.Sources),
			ExternalAccountCount: connectClampInt32(g.ExternalAccounts), UserCount: connectClampInt32(g.Users)}
		if g.OldestImportedAt != nil {
			row.OldestImportedAt = timestamppb.New(*g.OldestImportedAt)
		}
		out.Groups = append(out.Groups, row)
	}
	return out, nil
}

// ---- converters ----

func adminJobFromQueueItem(it domain.ImportQueueItem) *v1.AdminJob {
	out := &v1.AdminJob{
		Id: it.ID.String(), JobType: string(it.ItemType), Provider: it.Provider, UserId: it.UserID.String(),
		Status: jobStatusToProto(it.Status), Priority: priorityName(it.Priority), Attempt: connectClampInt32(it.Attempts),
		EnqueuedAt: timestamppb.New(it.CreatedAt), PayloadTypeUrl: "cairn.import_queue." + string(it.ItemType),
	}
	acct := it.ExternalAccountID.String()
	out.ExternalAccountId = &acct
	if it.StartedAt != nil {
		out.StartedAt = timestamppb.New(*it.StartedAt)
	}
	if it.CompletedAt != nil {
		out.FinishedAt = timestamppb.New(*it.CompletedAt)
	}
	if it.LastError != "" {
		msg := it.LastError
		out.ErrorMessage = &msg
	}
	return out
}

func priorityName(p int) string {
	if p > 0 {
		return "high"
	}
	return "normal"
}

func jobStatusToProto(s domain.ImportItemStatus) v1.AdminJobStatus {
	switch s {
	case domain.ImportStatusPending:
		return v1.AdminJobStatus_ADMIN_JOB_STATUS_QUEUED
	case domain.ImportStatusInProgress:
		return v1.AdminJobStatus_ADMIN_JOB_STATUS_RUNNING
	case domain.ImportStatusDone:
		return v1.AdminJobStatus_ADMIN_JOB_STATUS_COMPLETED
	case domain.ImportStatusFailed:
		return v1.AdminJobStatus_ADMIN_JOB_STATUS_FAILED_DEAD_LETTER
	case domain.ImportStatusSkipped:
		return v1.AdminJobStatus_ADMIN_JOB_STATUS_CANCELLED
	}
	return v1.AdminJobStatus_ADMIN_JOB_STATUS_UNSPECIFIED
}

// jobStatusFromProto maps a wire status to the queue statuses it covers.
func jobStatusFromProto(s v1.AdminJobStatus) []domain.ImportItemStatus {
	switch s {
	case v1.AdminJobStatus_ADMIN_JOB_STATUS_QUEUED:
		return []domain.ImportItemStatus{domain.ImportStatusPending}
	case v1.AdminJobStatus_ADMIN_JOB_STATUS_RUNNING:
		return []domain.ImportItemStatus{domain.ImportStatusInProgress}
	case v1.AdminJobStatus_ADMIN_JOB_STATUS_COMPLETED:
		return []domain.ImportItemStatus{domain.ImportStatusDone}
	case v1.AdminJobStatus_ADMIN_JOB_STATUS_FAILED_RETRYING, v1.AdminJobStatus_ADMIN_JOB_STATUS_FAILED_DEAD_LETTER:
		return []domain.ImportItemStatus{domain.ImportStatusFailed}
	case v1.AdminJobStatus_ADMIN_JOB_STATUS_CANCELLED:
		return []domain.ImportItemStatus{domain.ImportStatusSkipped}
	}
	return nil
}

func deadLetterToProto(j domain.DeadLetteredJob) *v1.AdminJob {
	out := &v1.AdminJob{
		Id: j.ID.String(), JobType: j.Subject, Provider: providerOfSubject(j.Subject), Status: v1.AdminJobStatus_ADMIN_JOB_STATUS_FAILED_DEAD_LETTER,
		Priority: "normal", Attempt: connectClampInt32(j.DeliveredCount), EnqueuedAt: timestamppb.New(j.FirstSeenAt),
		FinishedAt: timestamppb.New(j.LastSeenAt), PayloadTypeUrl: "nats." + j.Stream,
	}
	if j.ReplayedAt != nil {
		out.Status = v1.AdminJobStatus_ADMIN_JOB_STATUS_QUEUED
	}
	if j.LastError != "" {
		msg := j.LastError
		out.ErrorMessage = &msg
	}
	if name := j.Consumer; name != "" {
		out.WorkerName = &name
	}
	return out
}

// providerOfSubject reads the provider token of cairn.<kind>.<job>.<provider>…
func providerOfSubject(subject string) string {
	parts := splitSubject(subject)
	if len(parts) >= 4 {
		return parts[3]
	}
	return ""
}

func subjectHasProvider(subject string, providers []string) bool {
	p := providerOfSubject(subject)
	for _, want := range providers {
		if want == p {
			return true
		}
	}
	return false
}

func splitSubject(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}
