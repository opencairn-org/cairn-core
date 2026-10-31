package audit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeRepo struct {
	port.AuditRepo
	entries []domain.AuditEntry
	err     error
}

func (f *fakeRepo) Record(_ context.Context, e domain.AuditEntry) error {
	if f.err != nil {
		return f.err
	}
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeRepo) List(context.Context, domain.AuditFilter) ([]domain.AuditEntry, error) {
	return f.entries, f.err
}

func kindOf(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

func TestRecorder_RecordsActorAndSeverity(t *testing.T) {
	repo := &fakeRepo{}
	r := &Recorder{Repo: repo}
	u := domain.User{ID: domain.UserID(uuid.New()), Username: "alice"}
	actor := UserActor(u)
	actor.IP = "10.0.0.1"

	r.Info(context.Background(), actor, "user.login", "user", u.ID.String(), map[string]string{"k": "v"})
	r.High(context.Background(), Actor{}, "instance.config", "instance", "", nil)

	if len(repo.entries) != 2 {
		t.Fatalf("entries = %d", len(repo.entries))
	}
	e := repo.entries[0]
	if e.ActorUserID == nil || *e.ActorUserID != u.ID || e.ActorUsername != "alice" || e.IPAddress != "10.0.0.1" {
		t.Errorf("actor not carried: %+v", e)
	}
	if e.Severity != domain.AuditInfo || e.Action != "user.login" || e.Metadata["k"] != "v" {
		t.Errorf("entry: %+v", e)
	}
	if s := repo.entries[1]; s.Severity != domain.AuditHigh || s.ActorUserID != nil {
		t.Errorf("system entry: %+v", s)
	}
}

func TestRecorder_FailureIsLoggedNotReturned(t *testing.T) {
	var buf bytes.Buffer
	r := &Recorder{Repo: &fakeRepo{err: errors.New("db down")}, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	r.Info(context.Background(), Actor{}, "x.y", "x", "1", nil)
	if !strings.Contains(buf.String(), "audit record failed") {
		t.Errorf("failure not logged: %q", buf.String())
	}
	// no logger must not panic either
	(&Recorder{Repo: &fakeRepo{err: errors.New("db down")}}).Info(context.Background(), Actor{}, "x.y", "x", "1", nil)
}

func TestRecorder_NilSafe(t *testing.T) {
	var r *Recorder
	r.Info(context.Background(), Actor{}, "x.y", "x", "1", nil)
	(&Recorder{}).High(context.Background(), Actor{}, "x.y", "x", "1", nil)

	if _, err := r.List(context.Background(), domain.AuditFilter{}); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Errorf("nil List: %v", err)
	}
}

func TestRecorder_List(t *testing.T) {
	repo := &fakeRepo{entries: []domain.AuditEntry{{Action: "a"}}}
	got, err := (&Recorder{Repo: repo}).List(context.Background(), domain.AuditFilter{})
	if err != nil || len(got) != 1 {
		t.Fatalf("List = %v, %v", got, err)
	}
	repo.err = errors.New("boom")
	if _, err := (&Recorder{Repo: repo}).List(context.Background(), domain.AuditFilter{}); kindOf(t, err) != port.ActionErrorInternal {
		t.Errorf("repo error: %v", err)
	}
}

func TestActor_UserIDOrZero(t *testing.T) {
	if (Actor{}).UserIDOrZero() != (domain.UserID{}) {
		t.Error("system actor not zero")
	}
	id := domain.UserID(uuid.New())
	if (Actor{UserID: &id}).UserIDOrZero() != id {
		t.Error("user id lost")
	}
}
