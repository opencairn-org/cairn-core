// Package instance holds the instance-wide admin use cases: settings, the
// outbound-email check and the health summary.
package instance

import (
	"context"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

func internalErr(op string, err error) error {
	return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("%s: %v", op, err))
}

// Settings reads and edits the instance settings row.
type Settings struct {
	Repo  port.InstanceSettingsRepo
	Audit *audit.Recorder
}

// Get returns the current settings.
func (s *Settings) Get(ctx context.Context) (domain.InstanceSettings, error) {
	cur, err := s.Repo.Get(ctx)
	if err != nil {
		return domain.InstanceSettings{}, internalErr("load instance settings", err)
	}
	return cur, nil
}

// Update applies the patch, validates and stores it; the diff is audited.
func (s *Settings) Update(ctx context.Context, actor audit.Actor, p domain.InstanceSettingsPatch) (domain.InstanceSettings, error) {
	cur, err := s.Get(ctx)
	if err != nil {
		return domain.InstanceSettings{}, err
	}
	diff, err := cur.Apply(p)
	if err != nil {
		return domain.InstanceSettings{}, port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	if len(diff) == 0 {
		return cur, nil
	}
	if err := s.Repo.Save(ctx, cur); err != nil {
		return domain.InstanceSettings{}, internalErr("save instance settings", err)
	}
	s.Audit.Record(ctx, actor, domain.AuditInstanceSettings, domain.AuditHigh, "instance_settings", "1", diff, nil)
	return s.Get(ctx)
}

// TestEmail sends a synthetic message through the configured adapter.
type TestEmail struct {
	Mail port.EmailSender
	Name string // instance name for the subject
}

// Send returns how long dispatch took; a delivery error is returned as-is
// so the caller can show it (it is not an action error).
func (t *TestEmail) Send(ctx context.Context, to string) (time.Duration, error) {
	if t.Mail == nil {
		return 0, port.NewActionError(port.ActionErrorUnavailable, "no email adapter is configured")
	}
	addr, err := domain.NormaliseEmail(to)
	if err != nil {
		return 0, port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	name := t.Name
	if name == "" {
		name = "Cairn"
	}
	start := time.Now()
	err = t.Mail.Send(ctx, port.EmailMessage{
		To: addr, Subject: name + ": test email",
		TextBody: "This is a test message from your " + name + " instance. Outbound email works.\n",
		HTMLBody: "<p>This is a test message from your " + name + " instance. Outbound email works.</p>",
	})
	return time.Since(start), err
}
