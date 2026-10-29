package instance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

type fakeSettingsRepo struct {
	port.InstanceSettingsRepo
	row   domain.InstanceSettings
	saved int
}

func (f *fakeSettingsRepo) Get(context.Context) (domain.InstanceSettings, error) { return f.row, nil }
func (f *fakeSettingsRepo) Save(_ context.Context, s domain.InstanceSettings) error {
	f.row = s
	f.saved++
	return nil
}

type auditSpy struct {
	port.AuditRepo
	n int
}

func (a *auditSpy) Record(context.Context, domain.AuditEntry) error { a.n++; return nil }

func kindOf(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

func TestSettingsUpdate(t *testing.T) {
	repo := &fakeSettingsRepo{row: domain.InstanceSettings{Name: "Cairn", URL: "http://localhost:8080", PrimaryColor: "#3b82f6", DefaultLocale: "en", DefaultTimezone: "UTC", DefaultUnits: domain.UserUnitsMetric, RequireInvite: true}}
	spy := &auditSpy{}
	s := &Settings{Repo: repo, Audit: &audit.Recorder{Repo: spy}}
	str := func(v string) *string { return &v }
	ctx := t.Context()

	if _, err := s.Update(ctx, audit.Actor{}, domain.InstanceSettingsPatch{Name: str(" ")}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("blank name: %v", err)
	}
	if _, err := s.Update(ctx, audit.Actor{}, domain.InstanceSettingsPatch{PrimaryColor: str("blue")}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("bad color: %v", err)
	}
	if _, err := s.Update(ctx, audit.Actor{}, domain.InstanceSettingsPatch{URL: str("not a url")}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("bad url: %v", err)
	}
	// No change: nothing saved or audited.
	if _, err := s.Update(ctx, audit.Actor{}, domain.InstanceSettingsPatch{Name: str("Cairn")}); err != nil || repo.saved != 0 || spy.n != 0 {
		t.Fatalf("no-op: %v saved=%d audited=%d", err, repo.saved, spy.n)
	}
	open := true
	domains := []string{" Example.com ", "", "b.org"}
	var quota *int64
	got, err := s.Update(ctx, audit.Actor{}, domain.InstanceSettingsPatch{Name: str("My Cairn"), RegistrationOpen: &open, AllowedEmailDomains: &domains, PerUserStorageQuotaBytes: &quota})
	if err != nil || got.Name != "My Cairn" || !got.RegistrationOpen || len(got.AllowedEmailDomains) != 2 || got.AllowedEmailDomains[0] != "example.com" || repo.saved != 1 || spy.n != 1 {
		t.Fatalf("update: %v %+v saved=%d audited=%d", err, got, repo.saved, spy.n)
	}
}

type fakeQueue struct {
	port.ImportQueueRepo
	counts map[domain.ImportItemStatus]int
}

func (f *fakeQueue) CountAllByStatus(context.Context, port.ImportQueueFilter) (map[domain.ImportItemStatus]int, error) {
	return f.counts, nil
}

type healthyBlobs struct{ port.BlobStore }

func (healthyBlobs) Healthy(context.Context) error { return nil }

type fakeDLQ struct {
	port.DeadLetterRepo
	open int
}

func (f *fakeDLQ) CountOpen(context.Context) (int, error) { return f.open, nil }

func TestHealthGrading(t *testing.T) {
	h := &Health{
		PingDB:  func(context.Context) error { return nil },
		QueueOK: func() (bool, string) { return true, "connected" },
		Blobs:   healthyBlobs{},
		Workers: func(context.Context) WorkerCounts { return WorkerCounts{Healthy: 1} },
		Queue:   &fakeQueue{counts: map[domain.ImportItemStatus]int{domain.ImportStatusPending: 3, domain.ImportStatusInProgress: 1}},
		Now:     func() time.Time { return time.Unix(0, 0) },
	}
	r := h.Check(t.Context())
	if r.Overall != Healthy || r.JobsQueued != 3 || r.JobsRunning != 1 || r.Storage.State != Healthy || r.Email.Detail != "not configured" {
		t.Fatalf("report %+v", r)
	}
	h.Blobs = nil
	if r := h.Check(t.Context()); r.Overall != Degraded || r.Storage.State != Degraded {
		t.Fatalf("no storage degrades: %+v", r)
	}
	h.Blobs = healthyBlobs{}
	h.DeadLetters = &fakeDLQ{open: 2}
	if r := h.Check(t.Context()); r.Overall != Degraded || r.JobsDLQ != 2 {
		t.Fatalf("dlq degrades: %+v", r)
	}
	h.PingDB = func(context.Context) error { return errors.New("down") }
	if r := h.Check(t.Context()); r.Overall != Unhealthy || r.Database.State != Unhealthy {
		t.Fatalf("db down: %+v", r)
	}
	h.PingDB = nil
	h.Workers = func(context.Context) WorkerCounts { return WorkerCounts{Offline: 2} }
	if r := h.Check(t.Context()); r.Database.State != Unhealthy {
		t.Fatalf("no db: %+v", r)
	}
}

type fakeMail struct {
	sent []port.EmailMessage
	err  error
}

func (f *fakeMail) Send(_ context.Context, m port.EmailMessage) error {
	f.sent = append(f.sent, m)
	return f.err
}

func TestTestEmail(t *testing.T) {
	if _, err := (&TestEmail{}).Send(t.Context(), "a@x.test"); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("no adapter: %v", err)
	}
	mail := &fakeMail{}
	te := &TestEmail{Mail: mail, Name: "Hill"}
	if _, err := te.Send(t.Context(), "nope"); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("bad address: %v", err)
	}
	if _, err := te.Send(t.Context(), "Admin@X.test"); err != nil || len(mail.sent) != 1 || mail.sent[0].To != "admin@x.test" || mail.sent[0].Subject != "Hill: test email" {
		t.Fatalf("send: %v %+v", err, mail.sent)
	}
	mail.err = errors.New("smtp down")
	if _, err := te.Send(t.Context(), "a@x.test"); err == nil || err.Error() != "smtp down" {
		t.Fatalf("delivery error passthrough: %v", err)
	}
}
