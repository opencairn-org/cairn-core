package account

import (
	"context"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Identities lists and unlinks OIDC identities. Passkeys may be nil.
type Identities struct {
	Repo      port.LinkedIdentityRepo
	Users     port.UserRepo
	Passkeys  port.PasskeyRepo
	Providers []domain.OIDCProvider
}

// List returns the user's links, oldest first.
func (i *Identities) List(ctx context.Context, userID domain.UserID) ([]domain.LinkedIdentity, error) {
	list, err := i.Repo.ListForUser(ctx, userID)
	if err != nil {
		return nil, internalErr("list identities", err)
	}
	return list, nil
}

// ProviderName returns the display label of a provider id (the id when unknown).
func (i *Identities) ProviderName(id string) string {
	for _, p := range i.Providers {
		if p.ID == id && p.Name != "" {
			return p.Name
		}
	}
	return id
}

// Unlink removes one link unless it is the account's last way to sign in.
func (i *Identities) Unlink(ctx context.Context, userID domain.UserID, id domain.LinkedIdentityID) error {
	list, err := i.Repo.ListForUser(ctx, userID)
	if err != nil {
		return internalErr("list identities", err)
	}
	found := false
	for _, li := range list {
		if li.ID == id {
			found = true
		}
	}
	if !found {
		return port.NewActionError(port.ActionErrorNotFound, "linked identity not found")
	}
	if len(list) == 1 {
		other, err := i.hasOtherCredential(ctx, userID)
		if err != nil {
			return err
		}
		if !other {
			return port.NewActionError(port.ActionErrorPrecondition,
				"cannot unlink the last sign-in method; set a password or add a passkey first")
		}
	}
	if err := i.Repo.Delete(ctx, userID, id); err != nil {
		return internalErr("unlink identity", err)
	}
	return nil
}

// hasOtherCredential reports whether the user can sign in without any OIDC link.
func (i *Identities) hasOtherCredential(ctx context.Context, userID domain.UserID) (bool, error) {
	u, err := i.Users.GetUser(ctx, userID)
	if err != nil {
		return false, internalErr("load user", err)
	}
	_, cred, err := i.Users.GetCredentialsByLogin(ctx, u.Username)
	if err != nil {
		return false, internalErr("load credentials", err)
	}
	if strings.TrimSpace(cred.PasswordHash) != "" {
		return true, nil
	}
	if i.Passkeys == nil {
		return false, nil
	}
	keys, err := i.Passkeys.ListByUser(ctx, userID)
	if err != nil {
		return false, internalErr("list passkeys", err)
	}
	return len(keys) > 0, nil
}
