package invite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

type memInvites struct {
	port.InviteRepo
	rows   map[domain.InviteID]*domain.Invite
	hashes map[domain.InviteID][]byte
}

func newMemInvites() *memInvites {
	return &memInvites{rows: map[domain.InviteID]*domain.Invite{}, hashes: map[domain.InviteID][]byte{}}
}

func (m *memInvites) Create(_ context.Context, inv domain.Invite, hash []byte) (domain.InviteID, error) {
	inv.ID = domain.InviteID(uuid.New())
	inv.CreatedAt = time.Now()
	m.rows[inv.ID], m.hashes[inv.ID] = &inv, hash
	return inv.ID, nil
}
func (m *memInvites) Get(_ context.Context, id domain.InviteID) (domain.Invite, error) {
	if inv, ok := m.rows[id]; ok {
		return *inv, nil
	}
	return domain.Invite{}, domain.ErrInviteNotFound
}
func (m *memInvites) ListForInstance(context.Context) ([]domain.Invite, error) {
	var out []domain.Invite
	for _, inv := range m.rows {
		out = append(out, *inv)
	}
	return out, nil
}
func (m *memInvites) Revoke(_ context.Context, id domain.InviteID, now time.Time) error {
	m.rows[id].RevokedAt = &now
	return nil
}
func (m *memInvites) ResetCode(_ context.Context, id domain.InviteID, hash []byte, prefix string, now time.Time) error {
	inv := m.rows[id]
	if inv.UsedAt != nil || inv.RevokedAt != nil || (inv.ExpiresAt != nil && !now.Before(*inv.ExpiresAt)) {
		return domain.ErrInviteInvalid
	}
	m.hashes[id], inv.CodePrefix = hash, prefix
	return nil
}

type mailSpy struct{ sent []port.EmailMessage }

func (m *mailSpy) Send(_ context.Context, msg port.EmailMessage) error {
	m.sent = append(m.sent, msg)
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

func TestInviteAdmin(t *testing.T) {
	repo := newMemInvites()
	mail := &mailSpy{}
	now := time.Now().UTC()
	a := &Admin{Invites: repo, Create: NewCreateInvite(repo, nil), Mail: mail, Instance: "Hill",
		SignupURL: func(code string) string { return "https://cairn.test/signup?code=" + code }, Now: func() time.Time { return now }}
	actor := audit.Actor{}
	ctx := t.Context()

	past := now.Add(-time.Hour)
	if _, err := a.Mint(ctx, actor, "", domain.UserRoleUser, &past, false); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("past expiry: %v", err)
	}
	if _, err := a.Mint(ctx, actor, "not-an-email", domain.UserRoleUser, nil, false); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("bad email: %v", err)
	}
	plain, err := a.Mint(ctx, actor, "", domain.UserRoleUser, nil, true)
	if err != nil || plain.Code == "" || plain.Delivered || len(mail.sent) != 0 {
		t.Fatalf("unbound invite never mails: %v %+v", err, plain)
	}
	bound, err := a.Mint(ctx, actor, "Bob@X.test", domain.UserRoleAdmin, nil, true)
	if err != nil || !bound.Delivered || len(mail.sent) != 1 || mail.sent[0].To != "bob@x.test" || bound.Invite.AssignedRole != domain.UserRoleAdmin {
		t.Fatalf("bound invite: %v %+v", err, bound)
	}
	oldHash := repo.hashes[bound.Invite.ID]
	resent, err := a.Resend(ctx, actor, bound.Invite.ID)
	if err != nil || !resent.Delivered || len(mail.sent) != 2 || string(repo.hashes[bound.Invite.ID]) == string(oldHash) {
		t.Fatalf("resend: %v %+v", err, resent)
	}
	if _, err := a.Resend(ctx, actor, plain.Invite.ID); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("resend unbound: %v", err)
	}
	if err := a.Revoke(ctx, actor, plain.Invite.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Resend(ctx, actor, plain.Invite.ID); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("resend revoked: %v", err)
	}
	if err := a.Revoke(ctx, actor, domain.InviteID(uuid.New())); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("revoke unknown: %v", err)
	}
	active, _ := a.List(ctx, false, false)
	all, _ := a.List(ctx, true, true)
	if len(active) != 1 || len(all) != 2 {
		t.Fatalf("list active=%d all=%d", len(active), len(all))
	}
}
