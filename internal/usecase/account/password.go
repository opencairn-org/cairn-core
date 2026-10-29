package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ResetTTL bounds a password-reset link.
const ResetTTL = time.Hour

// Passwords changes and resets passwords. Mail may be nil (reset requests
// then still answer generically so they never reveal whether an address exists).
type Passwords struct {
	Users    port.UserRepo
	Hasher   PasswordHasher
	Tokens   port.AuthTokenRepo
	Sessions port.SessionRepo
	Mail     port.EmailSender
	// ResetLink turns a reset code into the absolute URL the mail carries.
	ResetLink func(code string) string
	// Now is overridable for tests.
	Now func() time.Time
}

func (p *Passwords) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// Change verifies the current password, stores the new one and ends every
// other session of the user (keep is the calling session; zero ends all).
func (p *Passwords) Change(ctx context.Context, userID domain.UserID, keep domain.SessionID, current, next string) error {
	if err := domain.ValidatePassword(next); err != nil {
		return invalid(err)
	}
	u, err := p.Users.GetUser(ctx, userID)
	if err != nil {
		return internalErr("load user", err)
	}
	_, cred, err := p.Users.GetCredentialsByLogin(ctx, u.Username)
	if err != nil {
		return internalErr("load credentials", err)
	}
	if cred.PasswordHash == "" {
		return port.NewActionError(port.ActionErrorPrecondition, "this account has no password; use a reset link to set one")
	}
	ok, err := p.Hasher.Verify(current, cred.PasswordHash)
	if err != nil {
		return internalErr("verify password", err)
	}
	if !ok {
		return port.NewActionError(port.ActionErrorInvalid, "current password is incorrect")
	}
	encoded, err := p.Hasher.Hash(next)
	if err != nil {
		return internalErr("hash password", err)
	}
	if err := p.Users.UpdatePassword(ctx, userID, encoded); err != nil {
		return internalErr("update password", err)
	}
	if err := revokeOthers(ctx, p.Sessions, userID, keep); err != nil {
		return internalErr("revoke other sessions", err)
	}
	return nil
}

// RequestReset mails a reset link when the address belongs to an account and
// mail is configured. The returned flag only says whether a dispatch was
// attempted; callers answer identically either way (anti-enumeration).
func (p *Passwords) RequestReset(ctx context.Context, email string) (attempted bool, err error) {
	addr, err := domain.NormaliseEmail(email)
	if err != nil || p.Mail == nil || p.ResetLink == nil {
		return false, nil
	}
	user, err := p.Users.GetUserByEmail(ctx, addr)
	if err != nil {
		return false, nil // unknown address: say nothing
	}
	code, hash := newCode()
	if err := p.Tokens.CreatePasswordResetToken(ctx, user.ID, hash, p.now().Add(ResetTTL)); err != nil {
		return false, internalErr("create reset token", err)
	}
	link := p.ResetLink(code)
	// Delivery failures are logged by the caller, never surfaced to the requester.
	err = p.Mail.Send(ctx, port.EmailMessage{
		To:      user.Email,
		Subject: "Reset your Cairn password",
		TextBody: "Someone asked to reset the password for your Cairn account.\n\n" +
			"Open this link to choose a new password (valid for 1 hour):\n" + link + "\n\n" +
			"If you didn't request this, you can ignore this email.\n\n- Cairn",
		HTMLBody: "<p>Someone asked to reset the password for your Cairn account.</p>" +
			"<p><a href=\"" + link + "\">Choose a new password</a> (valid for 1 hour).</p>" +
			"<p>If you didn't request this, you can ignore this email.</p><p>- Cairn</p>",
	})
	return true, err
}

// ErrResetInvalid is the user-facing reason for a bad or expired reset code.
var ErrResetInvalid = errors.New("this reset link is invalid or has expired")

// ConfirmReset consumes a reset code, sets the password and ends every
// session of the user. The caller may then sign the user in.
func (p *Passwords) ConfirmReset(ctx context.Context, code, next string) (domain.UserID, error) {
	if err := domain.ValidatePassword(next); err != nil {
		return domain.UserID{}, invalid(err)
	}
	hash := sha256.Sum256([]byte(code))
	userID, err := p.Tokens.ConsumePasswordResetToken(ctx, hash[:], p.now())
	if errors.Is(err, port.ErrAuthTokenInvalid) {
		return domain.UserID{}, invalid(ErrResetInvalid)
	}
	if err != nil {
		return domain.UserID{}, internalErr("consume reset token", err)
	}
	encoded, err := p.Hasher.Hash(next)
	if err != nil {
		return domain.UserID{}, internalErr("hash password", err)
	}
	if err := p.Users.UpdatePassword(ctx, userID, encoded); err != nil {
		return domain.UserID{}, internalErr("update password", err)
	}
	if err := p.Sessions.RevokeAllForUser(ctx, userID); err != nil {
		return domain.UserID{}, internalErr("revoke sessions", err)
	}
	return userID, nil
}

// newCode returns a URL-safe random code plus the sha256 that gets stored.
func newCode() (code string, hash []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	code = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(code))
	return code, h[:]
}
