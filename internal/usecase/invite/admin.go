package invite

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

// Admin is the invite management behind AdminService and /api/admin/invites.
type Admin struct {
	Invites port.InviteRepo
	Create  *CreateInvite
	Mail    port.EmailSender
	// SignupURL turns a code into the link the invite email carries.
	SignupURL func(code string) string
	Instance  string
	Audit     *audit.Recorder
	Now       func() time.Time
}

func (a *Admin) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now().UTC()
}

// Created is an invite plus the code shown once.
type Created struct {
	Invite domain.Invite
	Code   string
	// Delivered says whether the invite mail was sent; Err carries the
	// delivery error when it was attempted and failed.
	Delivered bool
	Err       error
}

func actionInternal(op string, err error) error {
	return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("%s: %v", op, err))
}

// List returns invites newest first; used and expired ones are filtered out
// unless asked for (revoked ones count as used).
func (a *Admin) List(ctx context.Context, includeUsed, includeExpired bool) ([]domain.Invite, error) {
	all, err := a.Invites.ListForInstance(ctx)
	if err != nil {
		return nil, actionInternal("list invites", err)
	}
	now := a.now()
	out := make([]domain.Invite, 0, len(all))
	for _, inv := range all {
		switch inv.Status(now) {
		case "used", "revoked":
			if !includeUsed {
				continue
			}
		case "expired":
			if !includeExpired {
				continue
			}
		}
		out = append(out, inv)
	}
	return out, nil
}

// Mint creates an invite and, when asked and possible, mails it.
func (a *Admin) Mint(ctx context.Context, actor audit.Actor, email string, role domain.UserRole, expiresAt *time.Time, send bool) (Created, error) {
	if a.Create == nil {
		return Created{}, port.NewActionError(port.ActionErrorUnavailable, "invites are not available")
	}
	email = strings.TrimSpace(email)
	if email != "" {
		norm, err := domain.NormaliseEmail(email)
		if err != nil {
			return Created{}, port.NewActionError(port.ActionErrorInvalid, err.Error())
		}
		email = norm
	}
	var expiresIn time.Duration
	if expiresAt != nil {
		if expiresIn = expiresAt.Sub(a.now()); expiresIn <= 0 {
			return Created{}, port.NewActionError(port.ActionErrorInvalid, "expires_at must be in the future")
		}
	}
	res, err := a.Create.Execute(ctx, CreateInviteInput{Role: role, Email: email, ExpiresIn: expiresIn, CreatedBy: actor.UserID})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidUserRole) {
			return Created{}, port.NewActionError(port.ActionErrorInvalid, err.Error())
		}
		return Created{}, actionInternal("create invite", err)
	}
	inv, err := a.Invites.Get(ctx, res.ID)
	if err != nil {
		return Created{}, actionInternal("load invite", err)
	}
	out := Created{Invite: inv, Code: res.Code}
	if send {
		out.Delivered, out.Err = a.deliver(ctx, inv, res.Code)
	}
	a.Audit.Info(ctx, actor, domain.AuditInviteCreated, "invite", inv.ID.String(), map[string]string{"email": email, "role": string(inv.AssignedRole)})
	return out, nil
}

// Revoke retires an invite; already used or unknown ones are not found.
func (a *Admin) Revoke(ctx context.Context, actor audit.Actor, id domain.InviteID) error {
	if _, err := a.Invites.Get(ctx, id); err != nil {
		return port.NewActionError(port.ActionErrorNotFound, "invite not found")
	}
	if err := a.Invites.Revoke(ctx, id, a.now()); err != nil {
		return actionInternal("revoke invite", err)
	}
	a.Audit.Info(ctx, actor, domain.AuditInviteRevoked, "invite", id.String(), nil)
	return nil
}

// Resend reissues the code (the old one stops working) and mails the new
// link; it needs an email-bound, still-active invite.
func (a *Admin) Resend(ctx context.Context, actor audit.Actor, id domain.InviteID) (Created, error) {
	inv, err := a.Invites.Get(ctx, id)
	if err != nil {
		return Created{}, port.NewActionError(port.ActionErrorNotFound, "invite not found")
	}
	if inv.Email == "" {
		return Created{}, port.NewActionError(port.ActionErrorPrecondition, "the invite is not bound to an email address")
	}
	if a.Mail == nil {
		return Created{}, port.NewActionError(port.ActionErrorUnavailable, "no email adapter is configured")
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return Created{}, actionInternal("generate code", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(code))
	if err := a.Invites.ResetCode(ctx, id, sum[:], code[:8]+"…", a.now()); err != nil {
		if errors.Is(err, domain.ErrInviteInvalid) {
			return Created{}, port.NewActionError(port.ActionErrorPrecondition, "the invite is used, revoked or expired")
		}
		return Created{}, actionInternal("reset invite code", err)
	}
	inv.CodePrefix = code[:8] + "…"
	out := Created{Invite: inv, Code: code}
	out.Delivered, out.Err = a.deliver(ctx, inv, code)
	a.Audit.Info(ctx, actor, domain.AuditInviteResent, "invite", id.String(), map[string]string{"email": inv.Email})
	return out, nil
}

// deliver mails the signup link; (false, nil) when nothing could be sent.
func (a *Admin) deliver(ctx context.Context, inv domain.Invite, code string) (bool, error) {
	if a.Mail == nil || inv.Email == "" || a.SignupURL == nil {
		return false, nil
	}
	name := a.Instance
	if name == "" {
		name = "Cairn"
	}
	link := a.SignupURL(code)
	err := a.Mail.Send(ctx, port.EmailMessage{
		To:      inv.Email,
		Subject: "You're invited to " + name,
		TextBody: "You have been invited to join " + name + ".\n\nCreate your account here:\n" + link +
			"\n\nThe link is personal; do not forward it.\n\n- " + name,
		HTMLBody: "<p>You have been invited to join " + name + ".</p><p><a href=\"" + link + "\">Create your account</a></p>" +
			"<p>The link is personal; do not forward it.</p><p>- " + name + "</p>",
	})
	return err == nil, err
}
