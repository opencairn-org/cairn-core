// Package moderation holds the admin-side moderation use cases: content
// reports, hiding activities and federation domain blocks.
package moderation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

func internalErr(op string, err error) error {
	return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("%s: %v", op, err))
}

// ReportRow is a report with its reporter's username.
type ReportRow struct {
	domain.ContentReport
	ReporterUsername string
}

// Reports lists and resolves content reports and hides activities.
type Reports struct {
	Reports    port.ReportRepo
	Moderation port.ModerationRepo
	Users      port.UserReader
	Audit      *audit.Recorder
}

// List pages reports of one status (open by default) with reporter names.
func (r *Reports) List(ctx context.Context, status domain.ReportStatus, limit, offset int) ([]ReportRow, error) {
	if r.Reports == nil {
		return nil, port.NewActionError(port.ActionErrorUnavailable, "reports are not available")
	}
	if status == "" {
		status = domain.ReportOpen
	}
	if limit <= 0 {
		limit = 50
	}
	list, err := r.Reports.List(ctx, status, limit, max(0, offset))
	if err != nil {
		return nil, internalErr("list reports", err)
	}
	names := map[domain.UserID]string{}
	out := make([]ReportRow, 0, len(list))
	for _, rep := range list {
		name, ok := names[rep.ReporterID]
		if !ok && r.Users != nil {
			if u, err := r.Users.GetUser(ctx, rep.ReporterID); err == nil {
				name = u.Username
			}
			names[rep.ReporterID] = name
		}
		out = append(out, ReportRow{ContentReport: rep, ReporterUsername: name})
	}
	return out, nil
}

// File records a user's report; the reason is trimmed and capped.
func (r *Reports) File(ctx context.Context, reporter domain.UserID, kind domain.ReportTargetKind, target uuid.UUID, reason string) (domain.ContentReportID, error) {
	if r.Reports == nil {
		return domain.ContentReportID{}, port.NewActionError(port.ActionErrorUnavailable, "reports are not available")
	}
	if !kind.Valid() {
		return domain.ContentReportID{}, port.NewActionError(port.ActionErrorInvalid, "bad target_kind")
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > domain.MaxReportReasonLength {
		reason = reason[:domain.MaxReportReasonLength]
	}
	return r.Reports.Create(ctx, domain.ContentReport{ReporterID: reporter, TargetKind: kind, TargetID: target, Reason: reason})
}

// Resolve marks a report reviewed or dismissed.
func (r *Reports) Resolve(ctx context.Context, actor audit.Actor, id domain.ContentReportID, status domain.ReportStatus) (domain.ReportStatus, error) {
	if r.Reports == nil {
		return "", port.NewActionError(port.ActionErrorUnavailable, "reports are not available")
	}
	if status != domain.ReportDismissed {
		status = domain.ReportReviewed
	}
	if err := r.Reports.UpdateStatus(ctx, id, status, actor.UserIDOrZero()); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return "", port.NewActionError(port.ActionErrorNotFound, "report not found")
		}
		return "", internalErr("resolve report", err)
	}
	r.Audit.Info(ctx, actor, domain.AuditReportResolved, "report", id.String(), map[string]string{"status": string(status)})
	return status, nil
}

// SetActivityHidden hides or unhides an activity instance-wide.
func (r *Reports) SetActivityHidden(ctx context.Context, actor audit.Actor, activityID domain.ActivityID, hidden bool) error {
	if r.Moderation == nil {
		return port.NewActionError(port.ActionErrorUnavailable, "moderation is not available")
	}
	if err := r.Moderation.SetActivityHidden(ctx, activityID, hidden); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return port.NewActionError(port.ActionErrorNotFound, "activity not found")
		}
		return internalErr("hide activity", err)
	}
	r.Audit.High(ctx, actor, domain.AuditActivityHidden, "activity", activityID.String(), map[string]string{"hidden": fmt.Sprint(hidden)})
	return nil
}

// Blocks manages defederated domains.
type Blocks struct {
	Repo  port.FederationBlockRepo
	Audit *audit.Recorder
}

// List returns the blocked domains.
func (b *Blocks) List(ctx context.Context) ([]domain.BlockedDomain, error) {
	if b.Repo == nil {
		return nil, port.NewActionError(port.ActionErrorUnavailable, "federation is not enabled")
	}
	list, err := b.Repo.List(ctx)
	if err != nil {
		return nil, internalErr("list blocked domains", err)
	}
	return list, nil
}

// NormaliseDomain lowercases and validates a host name.
func NormaliseDomain(raw string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(raw))
	if d == "" || strings.ContainsAny(d, "/ :@?#") {
		return "", errors.New("domain must be a bare host name")
	}
	return d, nil
}

// Block defederates a domain (idempotent; refreshes the reason).
func (b *Blocks) Block(ctx context.Context, actor audit.Actor, rawDomain, reason string) (string, error) {
	if b.Repo == nil {
		return "", port.NewActionError(port.ActionErrorUnavailable, "federation is not enabled")
	}
	d, err := NormaliseDomain(rawDomain)
	if err != nil {
		return "", port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	if err := b.Repo.Block(ctx, d, reason); err != nil {
		return "", internalErr("block domain", err)
	}
	b.Audit.High(ctx, actor, domain.AuditFederationBlocked, "federation_domain", d, map[string]string{"reason": reason})
	return d, nil
}

// Unblock lifts a block.
func (b *Blocks) Unblock(ctx context.Context, actor audit.Actor, rawDomain string) error {
	if b.Repo == nil {
		return port.NewActionError(port.ActionErrorUnavailable, "federation is not enabled")
	}
	d, err := NormaliseDomain(rawDomain)
	if err != nil {
		return port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	if err := b.Repo.Unblock(ctx, d); err != nil {
		return internalErr("unblock domain", err)
	}
	b.Audit.High(ctx, actor, domain.AuditFederationUnblocked, "federation_domain", d, nil)
	return nil
}
