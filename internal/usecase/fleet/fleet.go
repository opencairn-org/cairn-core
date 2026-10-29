// Package fleet is the admin view of the worker fleet: live workers, the
// pool registry and enrollments (AdminService and /api/admin/workers…).
package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
	"github.com/opencairn-org/cairn-core/internal/usecase/enrollment"
)

// MaxEnrollmentTTL caps how long an enrollment token stays valid.
const MaxEnrollmentTTL = 365 * 24 * time.Hour

// Fleet composes the registries; Live reads presence (main-package KV).
type Fleet struct {
	Pools       port.WorkerPoolRepo
	Enrollments port.WorkerEnrollmentRepo
	Create      *enrollment.CreateWorkerEnrollment
	Revoke      *enrollment.RevokeWorkerEnrollment
	Live        func(ctx context.Context) []domain.WorkerHeartbeat
	Providers   []string
	Audit       *audit.Recorder
	Now         func() time.Time
}

func (f *Fleet) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now().UTC()
}

func internalErr(op string, err error) error {
	return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("%s: %v", op, err))
}

// Workers lists the live instances.
func (f *Fleet) Workers(ctx context.Context) []domain.WorkerHeartbeat {
	if f.Live == nil {
		return nil
	}
	return f.Live(ctx)
}

// PoolStatuses lists registered pools with how many instances are online.
func (f *Fleet) PoolStatuses(ctx context.Context) ([]domain.PoolStatus, error) {
	if f.Pools == nil {
		return nil, port.NewActionError(port.ActionErrorUnavailable, "the worker pool registry is not available")
	}
	pools, err := f.Pools.ListAll(ctx)
	if err != nil {
		return nil, internalErr("list pools", err)
	}
	online := map[string]int{}
	for _, w := range f.Workers(ctx) {
		online[w.Provider+"/"+w.WorkerName]++
	}
	out := make([]domain.PoolStatus, 0, len(pools))
	for _, p := range pools {
		out = append(out, domain.PoolStatus{WorkerPool: p, OnlineInstances: online[p.Provider+"/"+p.Name]})
	}
	return out, nil
}

// RetirePool marks a pool retired so its accounts get reassigned.
func (f *Fleet) RetirePool(ctx context.Context, actor audit.Actor, provider, name string) error {
	if f.Pools == nil {
		return port.NewActionError(port.ActionErrorUnavailable, "the worker pool registry is not available")
	}
	if err := f.Pools.Retire(ctx, provider, name); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return port.NewActionError(port.ActionErrorNotFound, "no such active pool")
		}
		return internalErr("retire pool", err)
	}
	f.Audit.High(ctx, actor, domain.AuditWorkerPoolRetired, "worker_pool", provider+"/"+name, nil)
	return nil
}

// ListEnrollments lists enrollments with the filter applied.
func (f *Fleet) ListEnrollments(ctx context.Context, provider string, includeRevoked, includeExpired bool) ([]domain.WorkerEnrollment, error) {
	if f.Enrollments == nil {
		return nil, port.NewActionError(port.ActionErrorUnavailable, "worker enrollment is not available")
	}
	list, err := f.Enrollments.ListEnrollments(ctx, port.ListEnrollmentsFilter{Provider: provider, IncludeRevoked: includeRevoked, IncludeExpired: includeExpired})
	if err != nil {
		return nil, internalErr("list enrollments", err)
	}
	return list, nil
}

// EnrollmentInput is a new worker identity.
type EnrollmentInput struct {
	Name, Provider, Note string
	ExpiresIn            time.Duration
}

// CreateEnrollment mints an enrollment; the token is returned once.
func (f *Fleet) CreateEnrollment(ctx context.Context, actor audit.Actor, in EnrollmentInput) (domain.WorkerEnrollment, string, error) {
	if f.Create == nil {
		return domain.WorkerEnrollment{}, "", port.NewActionError(port.ActionErrorUnavailable, "worker enrollment is not available")
	}
	if strings.TrimSpace(in.Name) == "" {
		return domain.WorkerEnrollment{}, "", port.NewActionError(port.ActionErrorInvalid, "name is required")
	}
	if in.ExpiresIn <= 0 || in.ExpiresIn > MaxEnrollmentTTL {
		in.ExpiresIn = MaxEnrollmentTTL
	}
	res, err := f.Create.Execute(ctx, enrollment.CreateEnrollmentInput{
		Name: in.Name, Provider: in.Provider, ExpiresIn: in.ExpiresIn, Note: in.Note, CreatedBy: actor.UserID,
	})
	if err != nil {
		if errors.Is(err, enrollment.ErrInvalidInput) {
			return domain.WorkerEnrollment{}, "", port.NewActionError(port.ActionErrorInvalid, strings.TrimPrefix(err.Error(), enrollment.ErrInvalidInput.Error()+": "))
		}
		return domain.WorkerEnrollment{}, "", internalErr("create enrollment", err)
	}
	f.Audit.High(ctx, actor, domain.AuditEnrollmentCreated, "worker_enrollment", res.Enrollment.ID.String(), map[string]string{"name": in.Name, "provider": in.Provider})
	return res.Enrollment, res.Token, nil
}

// RevokeEnrollment ends an enrollment.
func (f *Fleet) RevokeEnrollment(ctx context.Context, actor audit.Actor, id domain.WorkerEnrollmentID, reason string) error {
	if f.Revoke == nil {
		return port.NewActionError(port.ActionErrorUnavailable, "worker enrollment is not available")
	}
	if err := f.Revoke.Execute(ctx, enrollment.RevokeInput{EnrollmentID: id, By: actor.UserID, Reason: reason}); err != nil {
		if errors.Is(err, domain.ErrEnrollmentNotFound) || errors.Is(err, domain.ErrNotFound) {
			return port.NewActionError(port.ActionErrorNotFound, "enrollment not found")
		}
		return port.NewActionError(port.ActionErrorPrecondition, err.Error())
	}
	f.Audit.High(ctx, actor, domain.AuditEnrollmentRevoked, "worker_enrollment", id.String(), map[string]string{"reason": reason})
	return nil
}

// ProlongEnrollment moves the expiry out (re-arming a lapsed enrollment).
func (f *Fleet) ProlongEnrollment(ctx context.Context, actor audit.Actor, id domain.WorkerEnrollmentID, by time.Duration) (time.Time, error) {
	if f.Enrollments == nil {
		return time.Time{}, port.NewActionError(port.ActionErrorUnavailable, "worker enrollment is not available")
	}
	if by <= 0 || by > MaxEnrollmentTTL {
		by = MaxEnrollmentTTL
	}
	expires := f.now().Add(by)
	if err := f.Enrollments.ExtendEnrollment(ctx, id, expires); err != nil {
		if errors.Is(err, domain.ErrEnrollmentNotFound) || errors.Is(err, domain.ErrNotFound) {
			return time.Time{}, port.NewActionError(port.ActionErrorNotFound, "enrollment not found")
		}
		return time.Time{}, port.NewActionError(port.ActionErrorPrecondition, err.Error())
	}
	f.Audit.Info(ctx, actor, "worker_enrollment.prolonged", "worker_enrollment", id.String(), map[string]string{"expires_at": expires.Format(time.RFC3339)})
	return expires, nil
}
