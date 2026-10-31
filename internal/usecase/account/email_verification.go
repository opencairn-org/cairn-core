package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// VerificationTTL bounds an email-verification link.
const VerificationTTL = 24 * time.Hour

// ErrEmailUnavailable: the instance has no mail channel or the user no address.
var ErrEmailUnavailable = errors.New("email unavailable")

// EmailVerification mails and confirms email-verification links. Mail may be nil.
type EmailVerification struct {
	Users  port.UserProfileStore
	Tokens port.AuthTokenRepo
	Mail   port.EmailSender
	// Link turns a verification code into the absolute URL the mail carries.
	Link func(code string) string
	Now  func() time.Time
}

func (e *EmailVerification) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

// Send issues a verification token for the user's address and mails the link.
func (e *EmailVerification) Send(ctx context.Context, user domain.User) error {
	if e.Mail == nil || user.Email == "" {
		return ErrEmailUnavailable
	}
	code, hash := newCode()
	if err := e.Tokens.CreateEmailVerificationToken(ctx, user.ID, user.Email, hash, e.now().Add(VerificationTTL)); err != nil {
		return fmt.Errorf("create email verification token: %w", err)
	}
	link := e.Link(code)
	return e.Mail.Send(ctx, port.EmailMessage{
		To:      user.Email,
		Subject: "Verify your Cairn email",
		TextBody: "Welcome to Cairn! Confirm this email address by opening the link below " +
			"(valid for 24 hours):\n" + link + "\n\n- Cairn",
		HTMLBody: "<p>Welcome to Cairn! Confirm this email address:</p>" +
			"<p><a href=\"" + link + "\">Verify email</a> (valid for 24 hours).</p><p>- Cairn</p>",
	})
}

// Resend mails a new link to the caller unless the address is already verified.
func (e *EmailVerification) Resend(ctx context.Context, userID domain.UserID) (alreadyVerified bool, err error) {
	user, err := e.Users.GetUser(ctx, userID)
	if err != nil {
		return false, port.NewActionError(port.ActionErrorInternal, "user lookup failed")
	}
	if user.EmailVerified {
		return true, nil
	}
	if e.Mail == nil || user.Email == "" {
		return false, port.NewActionError(port.ActionErrorUnavailable, "email is not available on this instance/account")
	}
	if err := e.Send(ctx, user); err != nil {
		return false, port.NewActionError(port.ActionErrorUpstream, "send failed")
	}
	return false, nil
}

// Confirm consumes a verification code and marks its user's address verified.
func (e *EmailVerification) Confirm(ctx context.Context, code string) error {
	hash := sha256.Sum256([]byte(code))
	userID, _, err := e.Tokens.ConsumeEmailVerificationToken(ctx, hash[:], e.now())
	if err != nil {
		return err
	}
	return e.Users.UpdateEmailVerified(ctx, userID, true)
}
