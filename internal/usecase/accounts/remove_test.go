package accounts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

type fakeAccounts struct {
	port.ExternalAccountRepo
	deleted []domain.ExternalAccountID
	missing bool
}

func (f *fakeAccounts) DeleteAccount(_ context.Context, id domain.ExternalAccountID) error {
	if f.missing {
		return domain.ErrNotFound
	}
	f.deleted = append(f.deleted, id)
	return nil
}

type recordingAudit struct {
	port.AuditRepo
	entries []domain.AuditEntry
}

func (r *recordingAudit) Record(_ context.Context, e domain.AuditEntry) error {
	r.entries = append(r.entries, e)
	return nil
}

func kindOf(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

func TestRemoverDetachesAndSettles(t *testing.T) {
	ctx := t.Context()
	user := domain.UserID(uuid.New())
	acct := domain.ExternalAccount{ID: domain.ExternalAccountID(uuid.New()), UserID: user, Provider: "strava"}
	other := domain.ExternalAccountID(uuid.New())
	acts := inmem.NewActivityRepo()
	shared, exclusive, foreign := domain.ActivityID(uuid.New()), domain.ActivityID(uuid.New()), domain.ActivityID(uuid.New())
	for _, id := range []domain.ActivityID{shared, exclusive, foreign} {
		if err := acts.SaveActivity(ctx, domain.Activity{ID: id, UserID: user}); err != nil {
			t.Fatal(err)
		}
	}
	src := func(act domain.ActivityID, a domain.ExternalAccountID) domain.ActivitySource {
		return domain.ActivitySource{ID: domain.SourceID(uuid.New()), ActivityID: act, UserID: user, Provider: "p", ExternalAccountID: &a, Status: domain.SourceStatusActive}
	}
	for _, s := range []domain.ActivitySource{src(shared, acct.ID), src(shared, other), src(exclusive, acct.ID), src(foreign, other)} {
		if err := acts.SaveSource(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	accounts := &fakeAccounts{}
	rec := &recordingAudit{}
	var recomputed []domain.ActivityID
	r := &Remover{Accounts: accounts, Activities: acts, Audit: &audit.Recorder{Repo: rec}, Now: func() time.Time { return time.Unix(0, 0) },
		Recompute: func(_ context.Context, id domain.ActivityID) error { recomputed = append(recomputed, id); return nil }}

	if _, err := (&Remover{}).Remove(ctx, audit.Actor{}, acct, false); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("unwired: %v", err)
	}
	res, err := r.Remove(ctx, audit.Actor{UserID: &user}, acct, true)
	if err != nil || res.DetachedSources != 2 || res.DeletedActivities != 1 || res.RecomputeFailures != 0 {
		t.Fatalf("remove: %v %+v", err, res)
	}
	if len(recomputed) != 1 || recomputed[0] != shared || len(accounts.deleted) != 1 || accounts.deleted[0] != acct.ID {
		t.Fatalf("settle: recomputed %v deleted %v", recomputed, accounts.deleted)
	}
	if live, _ := acts.ListSourcesForActivity(ctx, shared); len(live) != 1 || *live[0].ExternalAccountID != other {
		t.Fatalf("shared keeps the other source: %+v", live)
	}
	if all, _ := acts.ListAllSourcesForActivity(ctx, exclusive); len(all) != 1 || all[0].Status != domain.SourceStatusDetached || all[0].StatusReason != DetachReason {
		t.Fatalf("exclusive source detached: %+v", all)
	}
	if a, _ := acts.GetActivity(ctx, exclusive); a.DeletedAt == nil {
		t.Fatal("exclusive activity soft-deleted")
	}
	if a, _ := acts.GetActivity(ctx, foreign); a.DeletedAt != nil {
		t.Fatal("foreign activity untouched")
	}
	if len(rec.entries) != 1 || rec.entries[0].Action != domain.AuditExternalAccountRemoved || rec.entries[0].Metadata["detached_sources"] != "2" {
		t.Fatalf("audit: %+v", rec.entries)
	}

	// Without delete_exclusive_activities an orphaned activity stays.
	keep := domain.ActivityID(uuid.New())
	_ = acts.SaveActivity(ctx, domain.Activity{ID: keep, UserID: user})
	_ = acts.SaveSource(ctx, src(keep, acct.ID))
	if res, err := r.Remove(ctx, audit.Actor{}, acct, false); err != nil || res.DeletedActivities != 0 || res.DetachedSources != 1 {
		t.Fatalf("keep orphan: %v %+v", err, res)
	}
	if a, _ := acts.GetActivity(ctx, keep); a.DeletedAt != nil {
		t.Fatal("orphan kept")
	}
	accounts.missing = true
	if _, err := r.Remove(ctx, audit.Actor{}, acct, false); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("gone: %v", err)
	}
}
